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

// DataImportMode is the explicit discriminator that selects what a DataImport does with the imported
// bytes. It replaces the former polymorphic targetRef.kind discrimination.
// +kubebuilder:validation:Enum=PopulateVolume;ProduceArtifact
type DataImportMode string

const (
	// DataImportModePopulateVolume imports bytes into a volume that is preserved afterwards. The bytes
	// land either in a newly created PVC (pvcTemplate) or in an existing volume overwritten in place
	// (volumeRef + force). No durable artifact is produced; the volume itself is the product.
	DataImportModePopulateVolume DataImportMode = "PopulateVolume"
	// DataImportModeProduceArtifact materializes the data leg of an already-existing snapshot node: bytes
	// are staged into a transient scratch volume (scratchVolumeTemplate) and captured into a durable
	// VolumeSnapshotContent. snapshotRef identifies the owning node for the state-snapshotter
	// reverse-lookup; the DataImport controller itself does not read it.
	DataImportModeProduceArtifact DataImportMode = "ProduceArtifact"
)

// DataImportSpec is the desired state of a DataImport. The suffix convention is authoritative:
// ...Template is a spec for an object the import CREATES; ...Ref points at an object that already EXISTS.
//
// spec.mode is the explicit discriminator; the field sets valid on each mode are mutually exclusive and
// enforced by CRD CEL (see crds/dataimports.yaml) with a controller-side fail-closed guard:
//
//   - ProduceArtifact: snapshotRef (Ref) + scratchVolumeTemplate (Template) are required;
//     pvcTemplate/volumeRef/force are forbidden.
//   - PopulateVolume: exactly one of {pvcTemplate (Template), volumeRef (Ref)}; force is allowed only
//     together with volumeRef; snapshotRef/scratchVolumeTemplate are forbidden.
//
// +k8s:deepcopy-gen=true
type DataImportSpec struct {
	Ttl                  string `json:"ttl"`
	Publish              bool   `json:"publish,omitempty"`
	WaitForFirstConsumer bool   `json:"waitForFirstConsumer"`

	// Mode selects what the import does with the bytes. Defaults to PopulateVolume.
	// +kubebuilder:default=PopulateVolume
	// +optional
	Mode DataImportMode `json:"mode,omitempty"`

	// SnapshotRef (ProduceArtifact) references the ALREADY-EXISTING xxxSnapshot node the produced durable
	// artifact belongs to (apiVersion/kind/name; namespace implicit = the DataImport namespace). It is set
	// by the external creator (d8/user/backup); the DataImport controller does not read it — it exists so
	// the state-snapshotter reverse-lookup can match the leaf against spec.snapshotRef. Forbidden in
	// PopulateVolume.
	// +optional
	SnapshotRef *ObjectReference `json:"snapshotRef,omitempty"`
	// ScratchVolumeTemplate (ProduceArtifact) describes the transient scratch volume the imported bytes are
	// staged into before being captured into the durable VolumeSnapshotContent. The scratch volume is
	// destroyed after capture. Its StorageClass must be snapshot-capable. Forbidden in PopulateVolume.
	// +optional
	ScratchVolumeTemplate *ScratchVolumeSpec `json:"scratchVolumeTemplate,omitempty"`

	// PvcTemplate (PopulateVolume, create) fully specifies a PVC to create and populate; the PVC is
	// preserved after the import. Mutually exclusive with volumeRef. Forbidden in ProduceArtifact.
	// +optional
	PvcTemplate *PersistentVolumeClaimTemplateSpec `json:"pvcTemplate,omitempty"`
	// VolumeRef (PopulateVolume, overwrite) references an EXISTING volume (kind=PersistentVolumeClaim) to
	// overwrite in place; force must be set to acknowledge the destructive overwrite. Mutually exclusive
	// with pvcTemplate. Forbidden in ProduceArtifact.
	// +optional
	VolumeRef *ObjectReference `json:"volumeRef,omitempty"`
	// Force acknowledges the destructive in-place overwrite of an existing volume; it is only valid
	// together with volumeRef.
	// +optional
	Force bool `json:"force,omitempty"`
}

// EffectiveMode returns the import mode, defaulting an empty value to PopulateVolume (the CRD default) so
// controller logic never has to special-case the unset field.
func (s DataImportSpec) EffectiveMode() DataImportMode {
	if s.Mode == "" {
		return DataImportModePopulateVolume
	}
	return s.Mode
}

// ScratchVolumeSpec parameterizes the transient scratch volume used by a ProduceArtifact import. The bytes
// are staged into a PVC shaped from these parameters and then captured into a durable
// VolumeSnapshotContent; the scratch PVC is destroyed afterwards. StorageClassName and Size are required;
// VolumeMode defaults to Filesystem when empty.
// +k8s:deepcopy-gen=true
type ScratchVolumeSpec struct {
	// StorageClassName is the StorageClass of the scratch PVC. It must be snapshot-capable (its driver has
	// a VolumeSnapshotClass); the produced durable artifact is always a VolumeSnapshotContent.
	StorageClassName string `json:"storageClassName"`
	// Size is the requested scratch-PVC size (a Kubernetes quantity, e.g. "10Gi").
	Size string `json:"size"`
	// VolumeMode is the scratch-PVC volume mode (Block or Filesystem); defaults to Filesystem when empty.
	// +kubebuilder:validation:Enum=Block;Filesystem
	// +optional
	VolumeMode string `json:"volumeMode,omitempty"`
}

// +k8s:deepcopy-gen=true
type PersistentVolumeClaimTemplateSpec struct {
	PersistentVolumeClaimTemplateMetadata `json:"metadata,omitempty"`
	PersistentVolumeClaimSpec             `json:"spec,omitempty"`
}

// +k8s:deepcopy-gen=true
type PersistentVolumeClaimTemplateMetadata struct {
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
