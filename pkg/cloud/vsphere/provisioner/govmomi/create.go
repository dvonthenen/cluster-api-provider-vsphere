package govmomi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"time"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/klog"
	vsphereconfigv1 "sigs.k8s.io/cluster-api-provider-vsphere/pkg/apis/vsphereproviderconfig/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/constants"
	vpshereprovisionercommon "sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/provisioner/common"
	vsphereutils "sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/utils"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	clustererror "sigs.k8s.io/cluster-api/pkg/controller/error"
	apierrors "sigs.k8s.io/cluster-api/pkg/errors"
	"sigs.k8s.io/cluster-api/pkg/util"
)

func (pv *Provisioner) Create(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	if cluster == nil {
		return errors.New(constants.ClusterIsNullErr)
	}

	klog.V(4).Infof("govmomi.Actuator.Create %s", machine.Name)
	s, err := pv.sessionFromProviderConfig(cluster, machine)
	if err != nil {
		return err
	}
	createctx, cancel := context.WithCancel(*s.context)
	defer cancel()
	usersession, err := s.session.SessionManager.UserSession(createctx)
	if err != nil {
		return err
	}
	klog.V(4).Infof("Using session %v", usersession)
	task := vsphereutils.GetActiveTasks(machine)
	if task != "" {
		// In case an active task is going on, wait for its completion
		return pv.verifyAndUpdateTask(s, machine, task)
	}
	// Before going for cloning, check if we can locate a VM with the InstanceUUID
	// as this Machine. If found, that VM is the right match for this machine
	vmRef, err := pv.findVMByInstanceUUID(ctx, s, machine)
	if err != nil {
		return err
	}
	if vmRef != "" {
		pv.eventRecorder.Eventf(machine, corev1.EventTypeNormal, "Created", "Created Machine %s(%s)", machine.Name, vmRef)
		// Update the Machine object with the VM Reference annotation
		_, err := pv.updateVMReference(machine, vmRef)
		if err != nil {
			return err
		}
	}

	return pv.cloneVirtualMachine(s, cluster, machine)
}

func (pv *Provisioner) findVMByInstanceUUID(ctx context.Context, s *SessionContext, machine *clusterv1.Machine) (string, error) {
	klog.V(4).Infof("Trying to check existence of the VM via InstanceUUID %s", machine.UID)
	si := object.NewSearchIndex(s.session.Client)
	instanceUUID := true
	vmRef, err := si.FindByUuid(ctx, nil, string(machine.UID), true, &instanceUUID)
	if err != nil {
		return "", fmt.Errorf("error quering virtual machine or template using FindByUuid: %s", err)
	}
	if vmRef != nil {
		return vmRef.Reference().Value, nil
	}
	return "", nil
}

func (pv *Provisioner) verifyAndUpdateTask(s *SessionContext, machine *clusterv1.Machine, taskmoref string) error {
	ctx, cancel := context.WithCancel(*s.context)
	defer cancel()
	// If a task does exist on the
	var taskmo mo.Task
	taskref := types.ManagedObjectReference{
		Type:  "Task",
		Value: taskmoref,
	}
	err := s.session.RetrieveOne(ctx, taskref, []string{"info"}, &taskmo)
	if err != nil {
		// The task does not exist any more, thus no point tracking it. Thus clear it from the machine
		return pv.setTaskRef(machine, "")
	}
	switch taskmo.Info.State {
	// Queued or Running
	case types.TaskInfoStateQueued, types.TaskInfoStateRunning:
		// Requeue the machine update to check back in 5 seconds on the task
		return &clustererror.RequeueAfterError{RequeueAfter: time.Second * 5}
	// Successful
	case types.TaskInfoStateSuccess:
		if taskmo.Info.DescriptionId == "VirtualMachine.clone" {
			vmref := taskmo.Info.Result.(types.ManagedObjectReference)
			pv.eventRecorder.Eventf(machine, corev1.EventTypeNormal, "Created", "Created Machine %s(%s)", machine.Name, vmref.Value)
			// Update the Machine object with the VM Reference annotation
			updatedmachine, err := pv.updateVMReference(machine, vmref.Value)
			if err != nil {
				return err
			}
			// This is needed otherwise the update status on the original machine object would fail as the resource has been updated by the previous call
			// Note: We are not mutating the object retrieved from the informer ever. The updatedmachine is the updated resource generated using DeepCopy
			// This would just update the reference to be the newer object so that the status update works
			machine = updatedmachine
			return pv.setTaskRef(machine, "")
		} else if taskmo.Info.DescriptionId == "VirtualMachine.reconfigure" {
			pv.eventRecorder.Eventf(machine, corev1.EventTypeNormal, "Reconfigured", "Reconfigured Machine %s", taskmo.Info.EntityName)
		}
		return pv.setTaskRef(machine, "")
	case types.TaskInfoStateError:
		if taskmo.Info.DescriptionId == "VirtualMachine.clone" {
			pv.eventRecorder.Eventf(machine, corev1.EventTypeNormal, "Failed", "Creation failed for Machine %v", machine.Name)
			// Clear the reference to the failed task so that the next reconcile loop can re-create it
			return pv.setTaskRef(machine, "")
		}
	default:
		klog.Warningf("unknown state %s for task %s detected", taskmoref, taskmo.Info.State)
		return fmt.Errorf("Unknown state %s for task %s detected", taskmoref, taskmo.Info.State)
	}
	return nil
}

// CloneVirtualMachine clones the template to a virtual machine.
func (pv *Provisioner) cloneVirtualMachine(s *SessionContext, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	ctx, cancel := context.WithCancel(*s.context)
	defer cancel()

	machineConfig, err := vsphereutils.GetMachineProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return err
	}

	dc, err := s.finder.DatacenterOrDefault(ctx, machineConfig.MachineSpec.Datacenter)
	if err != nil {
		return err
	}
	s.finder.SetDatacenter(dc)

	// Let's check to make sure we can find the template earlier on... Plus, we need
	// the cluster/host info if we want to deploy direct to the cluster/host.
	var src *object.VirtualMachine
	if vsphereutils.IsValidUUID(machineConfig.MachineSpec.VMTemplate) {
		// If the passed VMTemplate is a valid UUID, then first try to find it treating that as InstanceUUID
		// In case if are not able to locate a matching VM then fall back to searching using the VMTemplate
		// as a name
		klog.V(4).Infof("Trying to resolve the VMTemplate as InstanceUUID %s", machineConfig.MachineSpec.VMTemplate)
		si := object.NewSearchIndex(s.session.Client)
		instanceUUID := true
		templateref, err := si.FindByUuid(ctx, dc, machineConfig.MachineSpec.VMTemplate, true, &instanceUUID)
		if err != nil {
			return fmt.Errorf("error querying virtual machine or template using FindByUuid: %s", err)
		}
		if templateref != nil {
			src = object.NewVirtualMachine(s.session.Client, templateref.Reference())
		}
	}
	if src == nil {
		klog.V(4).Infof("Trying to resolve the VMTemplate as Name %s", machineConfig.MachineSpec.VMTemplate)
		src, err = s.finder.VirtualMachine(ctx, machineConfig.MachineSpec.VMTemplate)
		if err != nil {
			klog.Errorf("VirtualMachine finder failed. err=%s", err)
			return err
		}
	}

	host, err := src.HostSystem(ctx)
	if err != nil {
		klog.Errorf("HostSystem failed. err=%s", err)
		return err
	}
	hostProps, err := PropertiesHost(host)
	if err != nil {
		return fmt.Errorf("error fetching host properties: %s", err)
	}

	// Since it's assumed that the ResourcePool name has been provided in the config, if we
	// want to deploy directly to the cluster/host, then we need to override the ResourcePool
	// path before generating the Cloud Provider config. This is done below in:
	// getCloudInitUserData()
	// +--- getCloudProviderConfig()
	resourcePoolPath := ""
	if len(machineConfig.MachineSpec.ResourcePool) == 0 {

		resourcePoolPath = fmt.Sprintf("/%s/host/%s/Resource", machineConfig.MachineSpec.Datacenter, hostProps.Name)
		klog.Infof("Attempting to deploy directly to cluster/host RP: %s", resourcePoolPath)
	}

	// Fetch the user-data for the cloud-init first, so that we can fail fast before even trying to connect to pv
	userData, err := pv.getCloudInitUserData(cluster, machine, resourcePoolPath)
	if err != nil {
		// err returned by the getCloudInitUserData would be of type RequeueAfterError in case kubeadm is not ready yet
		return err
	}
	metaData, err := pv.getCloudInitMetaData(cluster, machine)
	if err != nil {
		// err returned by the getCloudInitMetaData would be of type RequeueAfterError in case kubeadm is not ready yet
		return err
	}

	var spec types.VirtualMachineCloneSpec
	klog.V(4).Infof("[cloneVirtualMachine]: Preparing clone spec for VM %s", machine.Name)
	klog.V(4).Infof("clone VM to folder %s", machineConfig.MachineSpec.VMFolder)
	vmFolder, err := s.finder.FolderOrDefault(ctx, machineConfig.MachineSpec.VMFolder)
	if err != nil {
		return err
	}

	ds, err := s.finder.DatastoreOrDefault(ctx, machineConfig.MachineSpec.Datastore)
	if err != nil {
		return err
	}
	spec.Location.Datastore = types.NewReference(ds.Reference())

	spec.Config = &types.VirtualMachineConfigSpec{}
	// Use the object UID as the instanceUUID for the VM
	spec.Config.InstanceUuid = string(machine.UID)
	diskUUIDEnabled := true
	spec.Config.Flags = &types.VirtualMachineFlagInfo{
		DiskUuidEnabled: &diskUUIDEnabled,
	}
	if machineConfig.MachineSpec.NumCPUs > 0 {
		spec.Config.NumCPUs = int32(machineConfig.MachineSpec.NumCPUs)
	}
	if machineConfig.MachineSpec.MemoryMB > 0 {
		spec.Config.MemoryMB = machineConfig.MachineSpec.MemoryMB
	}
	spec.Config.Annotation = fmt.Sprintf("Virtual Machine is part of the cluster %s managed by cluster-api", cluster.Name)
	spec.Location.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndConsolidate)

	vmProps, err := PropertiesVM(src)
	if err != nil {
		return fmt.Errorf("error fetching vm/template properties: %s", err)
	}

	if len(machineConfig.MachineSpec.ResourcePool) > 0 {
		pool, err := s.finder.ResourcePoolOrDefault(ctx, machineConfig.MachineSpec.ResourcePool)

		if _, ok := err.(*find.NotFoundError); ok {
			klog.Warningf("Failed to find ResourcePool=%s err=%s. Attempting to create it.", machineConfig.MachineSpec.ResourcePool, err)

			poolRoot, errRoot := host.ResourcePool(ctx)
			if errRoot != nil {
				klog.Errorf("Failed to find root ResourcePool. err=%s", errRoot)
				return errRoot
			}

			klog.Info("Creating ResourcePool using default values. These values can be modified after ResourcePool creation.")
			pool, err = poolRoot.Create(ctx, machineConfig.MachineSpec.ResourcePool, types.DefaultResourceConfigSpec())
			if err != nil {
				klog.Errorf("Create ResourcePool failed. err=%s", err)
				return err
			}
		}

		spec.Location.Pool = types.NewReference(pool.Reference())
	} else {
		klog.Infof("Attempting to use Host ResourcePool")
		pool, err := host.ResourcePool(ctx)

		if err != nil {
			klog.Errorf("Host ResourcePool failed. err=%s", err)
			return err
		}

		spec.Location.Pool = types.NewReference(pool.Reference())
	}
	spec.PowerOn = true

	if machineConfig.MachineSpec.VsphereCloudInit {
		// In case of vsphere cloud-init datasource present, set the appropriate extraconfig options
		var extraconfigs []types.BaseOptionValue
		extraconfigs = append(extraconfigs, &types.OptionValue{Key: "guestinfo.metadata", Value: metaData})
		extraconfigs = append(extraconfigs, &types.OptionValue{Key: "guestinfo.metadata.encoding", Value: "base64"})
		extraconfigs = append(extraconfigs, &types.OptionValue{Key: "guestinfo.userdata", Value: userData})
		extraconfigs = append(extraconfigs, &types.OptionValue{Key: "guestinfo.userdata.encoding", Value: "base64"})
		spec.Config.ExtraConfig = extraconfigs
	} else {
		// This case is to support backwords compatibility, where we are using the ubuntu cloud image ovf properties
		// to drive the cloud-init workflow. Once the vsphere cloud-init datastore is merged as part of the official
		// cloud-init, then we can potentially remove this flag from the spec as then all the native cloud images
		// available for the different distros will include this new datasource.
		// See (https://github.com/akutz/cloud-init-vmware-guestinfo/ - vmware cloud-init datasource) for details
		if vmProps.Config.VAppConfig == nil {
			return fmt.Errorf("this source VM lacks a vApp configuration and cannot have vApp properties set on it")
		}
		allProperties := vmProps.Config.VAppConfig.GetVmConfigInfo().Property
		var props []types.VAppPropertySpec
		for _, p := range allProperties {
			defaultValue := " "
			if p.DefaultValue != "" {
				defaultValue = p.DefaultValue
			}
			prop := types.VAppPropertySpec{
				ArrayUpdateSpec: types.ArrayUpdateSpec{
					Operation: types.ArrayUpdateOperationEdit,
				},
				Info: &types.VAppPropertyInfo{
					Key:   p.Key,
					Id:    p.Id,
					Value: defaultValue,
				},
			}
			if p.Id == "user-data" {
				prop.Info.Value = userData
			}
			if p.Id == "public-keys" {
				prop.Info.Value, err = pv.GetSSHPublicKey(cluster)
				if err != nil {
					return err
				}
			}
			if p.Id == "hostname" {
				prop.Info.Value = machine.Name
			}
			props = append(props, prop)
		}
		spec.Config.VAppConfig = &types.VmConfigSpec{
			Property: props,
		}
	}

	l := object.VirtualDeviceList(vmProps.Config.Hardware.Device)
	deviceSpecs := []types.BaseVirtualDeviceConfigSpec{}
	disks := l.SelectByType((*types.VirtualDisk)(nil))
	// For the disks listed under the MachineSpec.Disks property, they are used
	// only for resizing a maching disk on the template. Currently, no new disk
	// is added. Only the matched disks via the DiskLabel are resized. If the
	// MachineSpec.Disks is specified but none of the disks matched to the disks
	// present in the VM Template then error is returned. This is to avoid the
	// case when the user did want to resize but accidentally passed a wrong
	// disk label. A 100% matching of disks in not enforced as the user might be
	// interested in resizing only a subset of disks and thus we don't want to
	// force the user to list all the disk and sizes if they don't want to change
	// all.
	diskMap := func(diskSpecs []vsphereconfigv1.DiskSpec) map[string]int64 {
		diskMap := make(map[string]int64)
		for _, s := range diskSpecs {
			diskMap[s.DiskLabel] = s.DiskSizeGB
		}
		return diskMap
	}(machineConfig.MachineSpec.Disks)
	diskChange := false
	for _, dev := range disks {
		disk := dev.(*types.VirtualDisk)
		if newSize, ok := diskMap[disk.DeviceInfo.GetDescription().Label]; ok {
			if disk.CapacityInBytes > vsphereutils.GiBToByte(newSize) {
				return errors.New("[FATAL] Disk size provided should be more than actual disk size of the template. Please correct the machineSpec to proceed")
			}
			klog.V(4).Infof("[cloneVirtualMachine] Resizing the disk \"%s\" to new size \"%d\"", disk.DeviceInfo.GetDescription().Label, newSize)
			diskChange = true
			disk.CapacityInBytes = vsphereutils.GiBToByte(newSize)
			diskspec := &types.VirtualDeviceConfigSpec{}
			diskspec.Operation = types.VirtualDeviceConfigSpecOperationEdit
			diskspec.Device = disk
			deviceSpecs = append(deviceSpecs, diskspec)
		}
	}
	if !diskChange && len(machineConfig.MachineSpec.Disks) > 0 {
		klog.V(4).Info("[cloneVirtualMachine] No disks were resized while cloning from template")
		return fmt.Errorf("[FATAL] None of the disks specified in the MachineSpec matched with the disks on the template %s", machineConfig.MachineSpec.VMTemplate)
	}

	nics := l.SelectByType((*types.VirtualEthernetCard)(nil))
	// Remove any existing nics on the source vm
	for _, dev := range nics {
		nic := dev.(types.BaseVirtualEthernetCard).GetVirtualEthernetCard()
		nicspec := &types.VirtualDeviceConfigSpec{}
		nicspec.Operation = types.VirtualDeviceConfigSpecOperationRemove
		nicspec.Device = nic
		deviceSpecs = append(deviceSpecs, nicspec)
	}
	// Add new nics based on the user info
	nicid := int32(-100)
	for _, network := range machineConfig.MachineSpec.Networks {
		netRef, err := s.finder.Network(ctx, network.NetworkName)
		if err != nil {
			return err
		}
		nic := types.VirtualVmxnet3{}
		nic.Key = nicid
		nic.Backing, err = netRef.EthernetCardBackingInfo(ctx)
		if err != nil {
			return err
		}
		nicspec := &types.VirtualDeviceConfigSpec{}
		nicspec.Operation = types.VirtualDeviceConfigSpecOperationAdd
		nicspec.Device = &nic
		deviceSpecs = append(deviceSpecs, nicspec)
		nicid--
	}
	spec.Config.DeviceChange = deviceSpecs
	if pv.eventRecorder != nil { // TODO: currently supporting nil for testing
		pv.eventRecorder.Eventf(machine, corev1.EventTypeNormal, "Creating", "Creating Machine %v", machine.Name)
	}
	task, err := src.Clone(ctx, vmFolder, machine.Name, spec)
	klog.V(6).Infof("clone VM with spec %v", spec)
	if err != nil {
		return err
	}
	return pv.setTaskRef(machine, task.Reference().Value)
}

// PropertiesVM is a convenience method that wraps fetching the
// VirtualMachine MO from its higher-level object.
func PropertiesVM(vm *object.VirtualMachine) (*mo.VirtualMachine, error) {
	klog.V(4).Infof("[DEBUG] Fetching properties for VM %q", vm.InventoryPath)
	ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultAPITimeout)
	defer cancel()
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), nil, &props); err != nil {
		return nil, err
	}
	return &props, nil
}

// PropertiesHost is a convenience method that wraps fetching the
// HostSystem MO from its higher-level object.
func PropertiesHost(host *object.HostSystem) (*mo.HostSystem, error) {
	klog.V(4).Infof("[DEBUG] Fetching properties for host %q", host.InventoryPath)
	ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultAPITimeout)
	defer cancel()
	var props mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), nil, &props); err != nil {
		return nil, err
	}
	return &props, nil
}

func (vc *Provisioner) updateVMReference(machine *clusterv1.Machine, vmref string) (*clusterv1.Machine, error) {
	providerSpec, err := vsphereutils.GetMachineProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		klog.Infof("Error fetching MachineProviderConfig: %s", err)
		return machine, err
	}
	providerSpec.MachineRef = vmref
	// Set the Kind and APIVersion again since they are not returned
	// See the following Issues for details:
	// https://github.com/kubernetes/client-go/issues/308
	// https://github.com/kubernetes/kubernetes/issues/3030
	providerSpec.Kind = reflect.TypeOf(*providerSpec).Name()
	providerSpec.APIVersion = vsphereconfigv1.SchemeGroupVersion.String()
	newMachine := machine.DeepCopy()
	out, err := json.Marshal(providerSpec)
	if err != nil {
		klog.Infof("Error marshaling ProviderConfig: %s", err)
		return machine, err
	}
	newMachine.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: out}
	newMachine, err = vc.clusterV1alpha1.Machines(newMachine.Namespace).Update(newMachine)
	if err != nil {
		klog.Infof("Error in updating the machine ref: %s", err)
		return machine, err
	}
	return newMachine, nil
}

func (pv *Provisioner) setTaskRef(machine *clusterv1.Machine, taskref string) error {
	oldProviderStatus, err := vsphereutils.GetMachineProviderStatus(machine)
	if err != nil {
		return err
	}

	if oldProviderStatus != nil && oldProviderStatus.TaskRef == taskref {
		// Nothing to update
		return nil
	}
	newProviderStatus := &vsphereconfigv1.VsphereMachineProviderStatus{}
	// create a copy of the old status so that any other fields except the ones we want to change can be retained
	if oldProviderStatus != nil {
		newProviderStatus = oldProviderStatus.DeepCopy()
	}
	newProviderStatus.TaskRef = taskref
	newProviderStatus.LastUpdated = time.Now().UTC().String()
	out, err := json.Marshal(newProviderStatus)
	newMachine := machine.DeepCopy()
	newMachine.Status.ProviderStatus = &runtime.RawExtension{Raw: out}
	if pv.clusterV1alpha1 == nil { // TODO: currently supporting nil for testing
		return nil
	}
	_, err = pv.clusterV1alpha1.Machines(newMachine.Namespace).UpdateStatus(newMachine)
	if err != nil {
		klog.Infof("Error in updating the machine ref: %s", err)
		return err
	}
	return nil
}

func (pv *Provisioner) getCloudInitMetaData(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (string, error) {
	machineconfig, err := vsphereutils.GetMachineProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return "", err
	}
	metadata, err := vpshereprovisionercommon.GetCloudInitMetaData(machine.Name, machineconfig)
	if err != nil {
		return "", err
	}
	metadata = base64.StdEncoding.EncodeToString([]byte(metadata))
	return metadata, nil
}

func (pv *Provisioner) getCloudInitUserData(cluster *clusterv1.Cluster, machine *clusterv1.Machine,
	resourcePoolPath string) (string, error) {
	script, err := pv.getStartupScript(cluster, machine)
	if err != nil {
		return "", err
	}
	config, err := pv.getCloudProviderConfig(cluster, machine, resourcePoolPath)
	if err != nil {
		return "", err
	}
	publicKey, err := pv.GetSSHPublicKey(cluster)
	if err != nil {
		return "", err
	}
	machineconfig, err := vsphereutils.GetMachineProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return "", err
	}
	userdata, err := vpshereprovisionercommon.GetCloudInitUserData(
		vpshereprovisionercommon.CloudInitTemplate{
			Script:              script,
			IsMaster:            util.IsControlPlaneMachine(machine),
			CloudProviderConfig: config,
			SSHPublicKey:        publicKey,
			TrustedCerts:        machineconfig.MachineSpec.TrustedCerts,
			NTPServers:          machineconfig.MachineSpec.NTPServers,
		},
	)
	if err != nil {
		return "", err
	}
	userdata = base64.StdEncoding.EncodeToString([]byte(userdata))
	return userdata, nil
}

func (pv *Provisioner) getCloudProviderConfig(cluster *clusterv1.Cluster, machine *clusterv1.Machine,
	resourcePoolPath string) (string, error) {
	clusterConfig, err := vsphereutils.GetClusterProviderSpec(cluster.Spec.ProviderSpec)
	if err != nil {
		return "", err
	}
	machineconfig, err := vsphereutils.GetMachineProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return "", err
	}

	// cloud provider requires bare IP:port, so if it is parseable as a url with a scheme, then
	// strip the scheme and path.  Otherwise continue.  TODO replace with better input validation.
	var server string
	serverURL, err := url.Parse(clusterConfig.VsphereServer)
	if err == nil && serverURL.Host != "" {
		server = serverURL.Host
		klog.Infof("Extracted vSphere server url: %s", server)
	} else {
		server = clusterConfig.VsphereServer
		klog.Infof("Using input vSphere server url: %s", server)
	}

	// TODO(ssurana): revisit once we solve https://github.com/kubernetes-sigs/cluster-api-provider-vsphere/issues/60
	cpc := vpshereprovisionercommon.CloudProviderConfigTemplate{
		Datacenter:   machineconfig.MachineSpec.Datacenter,
		Server:       server,
		Insecure:     true, // TODO(ssurana): Needs to be a user input
		UserName:     clusterConfig.VsphereUser,
		Password:     clusterConfig.VspherePassword,
		ResourcePool: machineconfig.MachineSpec.ResourcePool,
		Datastore:    machineconfig.MachineSpec.Datastore,
		Network:      "",
	}
	if len(resourcePoolPath) > 0 {
		cpc.ResourcePool = resourcePoolPath
	}
	if len(machineconfig.MachineSpec.Networks) > 0 {
		cpc.Network = machineconfig.MachineSpec.Networks[0].NetworkName
	}

	cloudProviderConfig, err := vpshereprovisionercommon.GetCloudProviderConfigConfig(cpc)
	if err != nil {
		return "", err
	}
	cloudProviderConfig = base64.StdEncoding.EncodeToString([]byte(cloudProviderConfig))
	return cloudProviderConfig, nil
}

// Builds and returns the startup script for the passed machine and cluster.
// Returns the full path of the saved startup script and possible error.
func (pv *Provisioner) getStartupScript(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (string, error) {
	machineconfig, err := vsphereutils.GetMachineProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return "", pv.HandleMachineError(machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal providerSpec field: %v", err), constants.CreateEventAction)
	}
	preloaded := machineconfig.MachineSpec.Preloaded
	var startupScript string
	if util.IsControlPlaneMachine(machine) {
		if machine.Spec.Versions.ControlPlane == "" {
			return "", pv.HandleMachineError(machine, apierrors.InvalidMachineConfiguration(
				"invalid master configuration: missing Machine.Spec.Versions.ControlPlane"), constants.CreateEventAction)
		}
		parsedVersion, err := version.ParseSemantic(machine.Spec.Versions.ControlPlane)
		if err != nil {
			return "", err
		}

		startupScript, err = vpshereprovisionercommon.GetMasterStartupScript(
			vpshereprovisionercommon.TemplateParams{
				MajorMinorVersion: fmt.Sprintf("%d.%d", parsedVersion.Major(), parsedVersion.Minor()),
				Cluster:           cluster,
				Machine:           machine,
				Preloaded:         preloaded,
			},
		)
		if err != nil {
			return "", err
		}
	} else {
		clusterstatus, err := vsphereutils.GetClusterProviderStatus(cluster)
		if err != nil {
			klog.Infof("Error fetching cluster ProviderStatus field: %s", err)
			return "", err
		}
		if clusterstatus == nil || clusterstatus.APIStatus != vsphereconfigv1.ApiReady {
			duration := vsphereutils.GetNextBackOff()
			klog.Infof("Waiting for Kubernetes API Status to be \"Ready\". Retrying in %s", duration)
			return "", &clustererror.RequeueAfterError{RequeueAfter: duration}
		}
		kubeadmToken, err := pv.GetKubeadmToken(cluster)
		if err != nil {
			duration := vsphereutils.GetNextBackOff()
			klog.Infof("Error generating kubeadm token, will retry in %s error: %s", duration, err.Error())
			return "", &clustererror.RequeueAfterError{RequeueAfter: duration}
		}
		parsedVersion, err := version.ParseSemantic(machine.Spec.Versions.Kubelet)
		if err != nil {
			return "", err
		}
		startupScript, err = vpshereprovisionercommon.GetNodeStartupScript(
			vpshereprovisionercommon.TemplateParams{
				Token:             kubeadmToken,
				MajorMinorVersion: fmt.Sprintf("%d.%d", parsedVersion.Major(), parsedVersion.Minor()),
				Cluster:           cluster,
				Machine:           machine,
				Preloaded:         preloaded,
			},
		)
		if err != nil {
			return "", err
		}
	}
	startupScript = base64.StdEncoding.EncodeToString([]byte(startupScript))
	return startupScript, nil
}
