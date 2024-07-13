package common

import (
	"context"
	"fmt"
	"hash/fnv"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	provCommon "sigs.k8s.io/sig-storage-local-static-provisioner/pkg/common"
	provDeleter "sigs.k8s.io/sig-storage-local-static-provisioner/pkg/deleter"
)

// zhou: only one mount path /mnt/local-storage/odf-lvs.
//       Used in Filesystem type of volume, inherit from local-static-provisioner.

// GenerateMountMap is used to get a set of mountpoints that can be quickly looked up
func GenerateMountMap(runtimeConfig *provCommon.RuntimeConfig) (sets.String, error) {
	type empty struct{}
	mountPointMap := sets.NewString()
	// Retrieve list of mount points to iterate through discovered paths (aka files) below
	mountPoints, err := runtimeConfig.Mounter.List()
	if err != nil {
		return mountPointMap, fmt.Errorf("error retrieving mountpoints: %w", err)
	}
	// Put mount points into set for faster checks below
	for _, mp := range mountPoints {
		mountPointMap.Insert(mp.Path)
	}
	return mountPointMap, nil
}

// zhou: core function, provisioning PV

// CreateLocalPV is used to create a local PV against a symlink
// after passing the same validations against that symlink that local-static-provisioner uses
func CreateLocalPV(
	obj runtime.Object,
	runtimeConfig *provCommon.RuntimeConfig,
	cleanupTracker *provDeleter.CleanupStatusTracker,
	storageClass storagev1.StorageClass,
	mountPointMap sets.String,
	client client.Client,
	symLinkPath string,
	deviceName string,
	idExists bool,
	extraLabelsForPV map[string]string,
) error {
	useJob := false
	nodeLabels := runtimeConfig.Node.GetLabels()
	hostname, found := nodeLabels[corev1.LabelHostname]
	if !found {
		return fmt.Errorf("could node find label %q for node %q", corev1.LabelHostname, runtimeConfig.Node.GetName())
	}

	// zhou: "local-pv-xxx", xxx comes from hash of these arguments.

	pvName := GeneratePVName(filepath.Base(symLinkPath), runtimeConfig.Node.Name, storageClass.Name)

	// zhou: node affinity in PV.

	nodeAffinity := &corev1.VolumeNodeAffinity{
		Required: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelHostname,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{hostname},
						},
					},
				},
			},
		},
	}

	// zhou: like this example, part of ConfigMap.
	/*
			  storageClassMap: |
			    odf-lvs:
			      blockCleanerCommand: null
			      fsType: ""
			      hostDir: /mnt/local-storage/odf-lvs
			      mountDir: /mnt/local-storage/odf-lvs
			      namePattern: ""
		      volumeMode: Block
	*/

	mountConfig, found := runtimeConfig.DiscoveryMap[storageClass.GetName()]
	if !found {
		return fmt.Errorf("could not find config for storageClass: %q", storageClass.GetName())
	}

	desiredVolumeMode := corev1.PersistentVolumeMode(mountConfig.VolumeMode)

	actualVolumeMode, err := provCommon.GetVolumeMode(runtimeConfig.VolUtil, symLinkPath)
	if err != nil {
		return fmt.Errorf("could not read the device's volume mode from the node: %w", err)
	}

	if cleanupTracker.InProgress(pvName, useJob) {
		klog.InfoS("PV is still being cleaned, not going to recreate it", "pvName", pvName, "disk", deviceName)
		return nil
	}

	_, _, err = cleanupTracker.RemoveStatus(pvName, useJob)
	if err != nil {
		klog.ErrorS(err, "failed to remove cleanup status for PV", "pvName", pvName)
		return err
	}

	var capacityBytes int64
	switch actualVolumeMode {
	case corev1.PersistentVolumeBlock:

		// zhou: ordinary path

		capacityBytes, err = runtimeConfig.VolUtil.GetBlockCapacityByte(symLinkPath)
		if err != nil {
			return fmt.Errorf("could not read device capacity: %w", err)
		}
		if desiredVolumeMode == corev1.PersistentVolumeBlock && len(storageClass.MountOptions) != 0 {
			klog.Warningf("Path %q will be used to create block volume, "+
				"mount options %v will not take effect.", symLinkPath, storageClass.MountOptions)
		}
	case corev1.PersistentVolumeFilesystem:
		if desiredVolumeMode == corev1.PersistentVolumeBlock {
			return fmt.Errorf("path %q of filesystem mode cannot be used to create block volume", symLinkPath)
		}
		// Validate that this path is an actual mountpoint
		if !mountPointMap.Has(symLinkPath) {
			return fmt.Errorf("path %q is not an actual mountpoint", symLinkPath)
		}
		capacityBytes, err = runtimeConfig.VolUtil.GetFsCapacityByte("", symLinkPath)
		if err != nil {
			return fmt.Errorf("path %q fs stats error: %w", symLinkPath, err)
		}
		// totalCapacityFSBytes += capacityByte
	default:
		return fmt.Errorf("path %q has unexpected volume type %q", symLinkPath, actualVolumeMode)
	}

	// zhou: set PV label like:
	/*
		kubernetes.io/hostname: node01.example.local
		storage.openshift.com/owner-kind: LocalVolumeSet
		storage.openshift.com/owner-name: odf-lvs
		storage.openshift.com/owner-namespace: openshift-local-storage
	*/

	accessor, err := meta.Accessor(obj)
	if err != nil {

		return fmt.Errorf("could not get object metadata accessor from obj: %+v", obj)
	}

	name := accessor.GetName()
	namespace := accessor.GetNamespace()
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if len(name) == 0 || len(namespace) == 0 || len(kind) == 0 {
		return fmt.Errorf("name: %q, namespace: %q, or  kind: %q is empty for obj: %+v", name, namespace, kind, obj)
	}

	labels := map[string]string{
		corev1.LabelHostname:  hostname,
		PVOwnerKindLabel:      kind,
		PVOwnerNamespaceLabel: namespace,
		PVOwnerNameLabel:      name,
	}
	for key, value := range extraLabelsForPV {
		labels[key] = value
	}

	// zhou: set PV annotation like:
	/*
		pv.kubernetes.io/provisioned-by: local-volume-provisioner-node01.example.local
		storage.openshift.com/device-id: wwn-0x5ee228447f8ce3a9
		storage.openshift.com/device-name: sda
	*/

	annotations := map[string]string{
		PVDeviceNameLabel:           deviceName,
		provCommon.AnnProvisionedBy: runtimeConfig.Name,
	}
	if idExists {
		annotations[PVDeviceIDLabel] = filepath.Base(symLinkPath)
	}

	// zhou: set persistentVolumeReclaimPolicy

	var reclaimPolicy corev1.PersistentVolumeReclaimPolicy
	if storageClass.ReclaimPolicy == nil {
		klog.InfoS("no ReclaimPolicy set in storageclass, defaulting to delete",
			"storageClass", storageClass.Name)
		reclaimPolicy = corev1.PersistentVolumeReclaimDelete
	} else {
		reclaimPolicy = *storageClass.ReclaimPolicy
	}

	localPVConfig := &provCommon.LocalPVConfig{
		Name:            pvName,
		HostPath:        symLinkPath,
		Capacity:        RoundDownCapacityPretty(capacityBytes), // d.VolUtil.GetBlockCapacityByte(filePath)
		StorageClass:    storageClass.GetName(),
		ReclaimPolicy:   reclaimPolicy,      // fetch from storageClass (created by LocalVolumeSet operator controller)
		ProvisionerName: runtimeConfig.Name, // populate in runtimeconfig earlier
		VolumeMode:      desiredVolumeMode,
		MountOptions:    storageClass.MountOptions, // d.getMountOptionsFromStorageClass(class)
		SetPVOwnerRef:   false,                     // PV should not get deleted when node is removed
		NodeAffinity:    nodeAffinity,
	}
	fsType := mountConfig.FsType
	if desiredVolumeMode == corev1.PersistentVolumeFilesystem && fsType != "" {
		localPVConfig.FsType = &fsType
	}

	// zhou: generate expected PV

	newPV := provCommon.CreateLocalPVSpec(localPVConfig)

	// zhou: create PV

	existingPV := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvName}}

	klog.InfoS("creating PV", "pvName", pvName)
	opRes, err := controllerutil.CreateOrUpdate(context.TODO(), client, existingPV, func() error {
		if existingPV.CreationTimestamp.IsZero() {
			// operations for create
			newPV.DeepCopyInto(existingPV)
		}
		// operations for update only

		// pv object says block, but the path is fs (the oppposite is fine)
		if existingPV.Spec.VolumeMode != nil &&
			*existingPV.Spec.VolumeMode == corev1.PersistentVolumeBlock && actualVolumeMode == corev1.PersistentVolumeFilesystem {
			err := fmt.Errorf("PV requires block mode but path was in fs mode")
			klog.ErrorS(err, "incorrect VolumeMode", "pvName", pvName, "symLinkPath", symLinkPath)
			runtimeConfig.Recorder.Eventf(existingPV, corev1.EventTypeWarning, provCommon.EventVolumeFailedDelete, err.Error())
		}

		// replace labels if and only if they don't already exist
		InitMapIfNil(&existingPV.ObjectMeta.Labels)
		// remove deprecated labels
		for _, key := range DeprecatedLabels {
			delete(existingPV.Labels, key)
		}
		for labelKey := range labels {
			_, found := existingPV.ObjectMeta.Labels[labelKey]
			if !found {
				existingPV.ObjectMeta.Labels[labelKey] = labels[labelKey]
			}
		}
		InitMapIfNil(&existingPV.ObjectMeta.Annotations)
		for annotationKey := range annotations {
			_, found := existingPV.ObjectMeta.Annotations[annotationKey]
			if !found {
				existingPV.ObjectMeta.Annotations[annotationKey] = annotations[annotationKey]
			}
		}

		return nil
	})
	if opRes != controllerutil.OperationResultNone {
		klog.InfoS("PV changed", "pvName", pvName, "status", opRes)
	}

	return err
}

// GeneratePVName is used to generate a PV name based on the filename, node, and storageclass
// Important, this hash value should remain consistent, so this function should not be changed
// in a way that would change its output.
func GeneratePVName(file, node, class string) string {
	h := fnv.New32a()
	h.Write([]byte(file))
	h.Write([]byte(node))
	h.Write([]byte(class))
	// This is the FNV-1a 32-bit hash
	return fmt.Sprintf("local-pv-%x", h.Sum32())
}
