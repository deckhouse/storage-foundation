/*
Copyright 2025 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type DataImport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DataImportSpec         `json:"spec"`
	Status DataExportImportStatus `json:"status"`
}

// +kubebuilder:object:root=true
type DataImportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []DataImport `json:"items"`
}

// +k8s:deepcopy-gen=true
type DataImportSpec struct {
	Ttl                  string `json:"ttl"`
	Publish              bool   `json:"publish,omitempty"`
	WaitForFirstConsumer bool   `json:"waitForFirstConsumer"`
	// TargetRef is the polymorphic target of the import; its Kind selects the import mode:
	//   - Mode A (snapshot leaf import): Kind is a snapshot leaf kind (e.g. "VolumeSnapshot"); Group+Name
	//     identify the leaf and the root StorageClassName/Size/VolumeMode below describe the scratch PVC.
	//   - Mode B (standalone PVC import): Kind == "PersistentVolumeClaim"; TargetRef.PvcTemplate fully
	//     specifies the target PVC and the root volume parameters are unused.
	TargetRef DataImportTargetRefSpec `json:"targetRef"`
	// StorageClassName, Size and VolumeMode describe the scratch PVC the imported bytes are written into
	// in Mode A (snapshot leaf import). They are provided directly in the spec (by d8, mirrored from the
	// source xxxSnapshot.status) instead of being read from the leaf's captured PVC manifest — the
	// snapshot is no longer downloaded on import. The selected StorageClass must be snapshot-capable (its
	// driver has a VolumeSnapshotClass); the produced durable artifact is always a VolumeSnapshotContent.
	// PersistentVolume/Detach import is not supported in core. Unused in Mode B (the target PVC is fully
	// described by targetRef.pvcTemplate).
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
	// Size is the requested scratch-PVC size in Mode A (a Kubernetes quantity, e.g. "10Gi"). Unused in
	// Mode B.
	// +optional
	Size string `json:"size,omitempty"`
	// VolumeMode is the scratch-PVC volume mode (Block or Filesystem) in Mode A; defaults to Filesystem
	// when empty. Unused in Mode B.
	// +kubebuilder:validation:Enum=Block;Filesystem
	// +optional
	VolumeMode string `json:"volumeMode,omitempty"`
}

// DataImportTargetRefSpec is the polymorphic target reference of a DataImport. Its Kind selects the
// import mode:
//
//   - Mode A (snapshot leaf import): Kind is the kind of a namespaced snapshot leaf CR (e.g.
//     "VolumeSnapshot", "VirtualDiskSnapshot"). Group and Name identify the leaf; the namespace is
//     implicit (the DataImport's own namespace). This ref exists so the state-snapshotter common
//     controller can reverse-lookup the DataImport from the leaf (matching spec.targetRef); the
//     DataImport controller itself does not read the leaf. The produced durable artifact is a
//     VolumeSnapshotContent.
//   - Mode B (standalone PVC import): Kind == "PersistentVolumeClaim". PvcTemplate fully specifies the
//     target PVC the imported bytes are written into; Group/Name are unused. No durable artifact is
//     produced and the PVC is preserved after the import.
//
// +k8s:deepcopy-gen=true
type DataImportTargetRefSpec struct {
	// Kind selects the import mode: "PersistentVolumeClaim" -> Mode B (standalone PVC import via
	// pvcTemplate); any other kind is treated as a snapshot leaf kind -> Mode A.
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// Group is the API group of the leaf snapshot resource in Mode A (e.g. "snapshot.storage.k8s.io",
	// "virtualization.deckhouse.io"). The served version is resolved dynamically. Unused in Mode B.
	// +optional
	Group string `json:"group,omitempty"`
	// Name is the leaf object name in Mode A. Unused in Mode B (the PVC name comes from
	// pvcTemplate.metadata.name).
	// +optional
	Name string `json:"name,omitempty"`
	// PvcTemplate fully specifies the target PVC in Mode B (Kind == "PersistentVolumeClaim"). The PVC is
	// created from it and preserved after the import completes. Forbidden in Mode A.
	// +optional
	PvcTemplate *PersistentVolumeClaimTemplateSpec `json:"pvcTemplate,omitempty"`
}

// +k8s:deepcopy-gen=true
type PersistentVolumeClaimTemplateSpec struct {
	DataImportTargetRefMetaSpec `json:"metadata,omitempty"`
	PersistentVolumeClaimSpec   `json:"spec,omitempty"`
}

// +k8s:deepcopy-gen=true
type DataImportTargetRefMetaSpec struct {
	Name        string            `json:"name,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// +k8s:deepcopy-gen=true
type PersistentVolumeClaimSpec struct {
	AccessModes      []PersistentVolumeAccessMode `json:"accessModes,omitempty"`
	Resources        VolumeResourceRequirements   `json:"resources,omitempty"`
	StorageClassName *string                      `json:"storageClassName,omitempty"`
	VolumeMode       *PersistentVolumeMode        `json:"volumeMode,omitempty"`
}

// VolumeResourceRequirements describes the storage resource requirements for a volume.
// +k8s:deepcopy-gen=true
type VolumeResourceRequirements struct {
	Requests ResourceList `json:"requests,omitempty"`
}

// ResourceList is a set of (resource name, quantity) pairs.
type ResourceList map[ResourceName]resource.Quantity

// +enum
type PersistentVolumeAccessMode string

const (
	// can be mounted in read/write mode to exactly 1 host
	ReadWriteOnce PersistentVolumeAccessMode = "ReadWriteOnce"
	// can be mounted in read-only mode to many hosts
	ReadOnlyMany PersistentVolumeAccessMode = "ReadOnlyMany"
	// can be mounted in read/write mode to many hosts
	ReadWriteMany PersistentVolumeAccessMode = "ReadWriteMany"
	// can be mounted in read/write mode to exactly 1 pod
	// cannot be used in combination with other access modes
	ReadWriteOncePod PersistentVolumeAccessMode = "ReadWriteOncePod"
)

// PersistentVolumeMode describes how a volume is intended to be consumed, either Block or Filesystem.
// +enum
type PersistentVolumeMode string

const (
	// PersistentVolumeBlock means the volume will not be formatted with a filesystem and will remain a raw block device.
	PersistentVolumeBlock PersistentVolumeMode = "Block"
	// PersistentVolumeFilesystem means the volume will be or is formatted with a filesystem.
	PersistentVolumeFilesystem PersistentVolumeMode = "Filesystem"
)

// +enum
type ResourceName string

const (
	// Volume size, in bytes (e,g. 5Gi = 5GiB = 5 * 1024 * 1024 * 1024)
	ResourceStorage ResourceName = "storage"
)

func (di *DataImport) GetStatus() *DataExportImportStatus {
	return &di.Status
}
