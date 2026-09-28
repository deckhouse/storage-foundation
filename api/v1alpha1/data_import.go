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

// The whole DataImport object is intentionally NOT immutable: only spec.mode and
// spec.snapshotRef are locked after creation, by the CEL transition rules on
// DataImportSpec and on its snapshotRef. Other spec fields (e.g. ttl) may still
// change.
//
// The status is the DataExportImportStatus shared with DataExport; its
// DataImport-specific descriptions and enums are set here.

// Resource for importing data into a newly created PVC (CreatePVC) or producing a durable data
// artifact for an already-existing snapshot node (PopulateData).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=di
// +kubebuilder:metadata:labels={"heritage=deckhouse","module=storage-foundation","backup.deckhouse.io/cluster-config=true"}
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.url"
// +kubebuilder:printcolumn:name="Public URL",type="string",JSONPath=".status.publicURL"
// +kubebuilder:printcolumn:name="TTL",type="string",JSONPath=".spec.ttl",priority=1
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".spec.mode",priority=1
// +kubebuilder:printcolumn:name="Storage Class",type="string",JSONPath=".spec.storageParams.storageClassName",priority=1
// +kubebuilder:printcolumn:name="Wait For Consumer",type="boolean",JSONPath=".spec.waitForFirstConsumer",priority=1
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase",description="The status of the DataImport resource."
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +crd-enricher:crd:minimal=true
// +crd-enricher:crd:preserveUnknownFields=false
// +crd-enricher:raw:properties.status.description=DataImport resource status details.
// +crd-enricher:raw:properties.status.properties.url.description=Internal URL of the importer service.
// +crd-enricher:raw:properties.status.properties.ca.description=Base64-encoded CA certificate for establishing a TLS connection to the importer pod.
// +crd-enricher:raw:properties.status.properties.publicURL.description=Public URL of the importer service.
// +crd-enricher:raw:properties.status.properties.accessTimestamp.description=Last access timestamp updated by the importer pod.
// +crd-enricher:raw:properties.status.properties.volumeMode.description=Volume mode of the exported data.
// +crd-enricher:raw:properties.status.properties.phase.enum=[Pending, Ready, Completed, Expired, Failed, Terminating]
// +crd-enricher:raw:properties.status.properties.phase.description="Coarse-grained lifecycle state, written exclusively by the controller:\nPending → Ready → Completed | Expired | Failed. `Terminating` denotes an object with a\ndeletion timestamp and is a transient state, not an outcome. `Expired` is a normal\nterminal outcome (the idle-TTL window elapsed), not a failure."
// +crd-enricher:raw:properties.status.properties.completionTimestamp.description="Time the DataImport reached a terminal phase (Completed, Expired or Failed). Set once by\nthe controller; the garbage collector measures retention age from this timestamp."
// +crd-enricher:raw:properties.status.properties.serverState.enum=[Ready, Finished, IdleExpired]
// +crd-enricher:raw:properties.status.properties.serverState.description="Raw progress signal reported by the importer server pod (the only writer): `Ready` when\nthe server is serving, `Finished` when the client upload finished durably, `IdleExpired`\nwhen the idle-TTL window elapsed. The controller derives phase and conditions from it."
// +crd-enricher:raw:properties.status.properties.data.description="Captured-data block for this import. Carries the durable cluster-scoped data artifact\nunder data.artifactRef (a VolumeSnapshotContent). Populated once the backing\nVolumeCaptureRequest completes."
// +crd-enricher:raw:properties.status.properties.data.properties.artifactRef.description="Reference to the durable cluster-scoped data artifact produced by this import\n(a VolumeSnapshotContent)."
// +crd-enricher:raw:properties.status.properties.data.properties.artifactRef.properties.uid.description="UID of the durable data artifact (for example the VolumeSnapshotContent UID),\nmaking the reference self-contained. Optional; producers fill it best-effort."
// +crd-enricher:raw:properties.status.properties.data.properties.fsType.description="Filesystem the imported bytes were actually written onto, observed on the scratch\nvolume's PersistentVolume (`spec.csi.fsType`) before that volume was destroyed. The\nscratch volume is deleted right after capture and the artifact itself records no\nfilesystem type, so this is the only surviving record of it.\n\nEmpty means \"not known\", never \"default\": a Block import carries no filesystem at\nall, and a CSI driver may record no filesystem type on the volume."
// +crd-enricher:raw:properties.status.properties.conditions.items.description=Array of conditions describing the current state of the DataImport resource.
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.type.description=Condition type.
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.type.enum=[Ready, UploadFinished, Completed]
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.status.description=Condition status.
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.reason.description=Current condition status reason.
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.reason.enum=[Pending, PVCCreated, ServerReady, InProgress, Expired, Deleted, UploadFinished, Completed, Failed, TargetFailed, CleanupFailed]
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.message.description="Human-readable message indicating details about the transition.\n\nThis may be an empty string."
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.lastTransitionTime.description="Last time the condition transitioned from one status to another.\n\nThis should be when the underlying condition changed.\nIf the last transition time is not known, then using the time when the API field changed is acceptable."
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.observedGeneration.description="The `.metadata.generation` that the condition was set based upon.\n\nFor instance, if `.metadata.generation` is currently 12, but the `.status.conditions[x].observedGeneration` is 9, the condition is out of date.\nwith respect to the current state of the instance."
type DataImport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec DataImportSpec `json:"spec"`
	// +optional
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
// +kubebuilder:validation:Enum=CreatePVC;PopulateData
type DataImportMode string

const (
	// DataImportModeCreatePVC imports bytes into a newly created PVC (pvcTemplate) that is preserved
	// afterwards. No durable artifact is produced; the PVC itself is the product.
	DataImportModeCreatePVC DataImportMode = "CreatePVC"
	// DataImportModePopulateData materializes the data leg of an already-existing snapshot node: bytes
	// are staged into a transient scratch volume (storageParams) and captured into a durable
	// VolumeSnapshotContent. snapshotRef identifies the owning node for the state-snapshotter
	// reverse-lookup; the DataImport controller itself does not read it.
	DataImportModePopulateData DataImportMode = "PopulateData"
)

// DataImportSpec is the desired state of a DataImport. The suffix convention is authoritative:
// ...Template is a spec for an object the import CREATES; ...Ref points at an object that already EXISTS.
//
// spec.mode is the explicit discriminator; the field sets valid on each mode are mutually exclusive and
// enforced by the CEL rules below with a controller-side fail-closed guard:
//
//   - PopulateData: snapshotRef (Ref) + storageParams are required; pvcTemplate is forbidden.
//   - CreatePVC: pvcTemplate (Template) is required; snapshotRef/storageParams are forbidden.
//
// pvcTemplate must name the PVC it creates. mode is set once at creation and never changes: the
// controllers and the state-snapshotter reverse-lookup assume a stable mode for the object's lifetime.

// +k8s:deepcopy-gen=true
// +kubebuilder:validation:XValidation:rule="self.mode != 'PopulateData' || (has(self.snapshotRef) && has(self.storageParams))",message="mode PopulateData requires snapshotRef and storageParams."
// +kubebuilder:validation:XValidation:rule="self.mode != 'PopulateData' || !has(self.pvcTemplate)",message="mode PopulateData forbids pvcTemplate."
// +kubebuilder:validation:XValidation:rule="self.mode != 'CreatePVC' || has(self.pvcTemplate)",message="mode CreatePVC requires pvcTemplate."
// +kubebuilder:validation:XValidation:rule="self.mode != 'CreatePVC' || (!has(self.snapshotRef) && !has(self.storageParams))",message="mode CreatePVC forbids snapshotRef and storageParams."
// +kubebuilder:validation:XValidation:rule="!has(self.pvcTemplate) || (has(self.pvcTemplate.metadata) && has(self.pvcTemplate.metadata.name) && size(self.pvcTemplate.metadata.name) > 0)",message="pvcTemplate requires metadata.name."
// +kubebuilder:validation:XValidation:rule="self.mode == oldSelf.mode",message="mode is immutable"
type DataImportSpec struct {
	// Time-to-live duration after the last user request.
	//
	// Acceptable values:
	//
	// - `<number>s`
	// - `<number>m`
	// - `<number>h`
	//
	// For example, `5m`, `2h45m`, or `1d`.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?h)?([0-9]+(\.[0-9]+)?m)?([0-9]+s)?$`
	TTL string `json:"ttl"`
	// Expose the importer pod outside the cluster.
	// +optional
	// +kubebuilder:default=false
	Publish bool `json:"publish,omitempty"`
	// Controls what the import waits for before it starts moving data into the target PVC.
	// When the target PVC's StorageClass has `volumeBindingMode: WaitForFirstConsumer` and
	// this field is `true` (the default), the import waits until a pod that mounts the
	// target PVC is scheduled, then places the imported volume on the node that pod was
	// scheduled to. When `false`, the import does not wait for a consumer: it proceeds
	// immediately and becomes the volume's first consumer itself. Has no effect when the
	// target PVC's StorageClass does not delay binding until a consumer appears.
	// +optional
	// +kubebuilder:default=true
	WaitForFirstConsumer bool `json:"waitForFirstConsumer"`

	// Discriminator selecting what the import does with the bytes:
	//
	// - `CreatePVC`: write into a preserved, newly created PVC (`pvcTemplate`). No durable
	//   artifact.
	// - `PopulateData`: stage the bytes into a transient scratch volume (`storageParams`) and
	//   capture them into a durable VolumeSnapshotContent for the already-existing snapshot node
	//   referenced by `snapshotRef`.
	// +kubebuilder:default=CreatePVC
	// +optional
	Mode DataImportMode `json:"mode,omitempty"`

	// snapshotRef binds the produced artifact to an existing snapshot node; the state-snapshotter
	// reverse-lookup assumes a stable ref for the object's lifetime. ObjectReference is shared with
	// VolumeRestoreRequest, so the namespace and uid it also carries are taken out of this field's
	// schema: the namespace is implicitly the DataImport's own.

	// PopulateData only. Reference to the already-existing xxxSnapshot node the produced
	// durable artifact belongs to (namespace implicit = the DataImport namespace). Set by the
	// external creator (d8/user/backup); the DataImport controller does not read it — it exists
	// so the state-snapshotter reverse-lookup can match the leaf against `spec.snapshotRef`.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="snapshotRef is immutable"
	// +crd-enricher:unset:properties.namespace
	// +crd-enricher:unset:properties.uid
	// +crd-enricher:raw:required=[kind, name]
	// +crd-enricher:raw:properties.apiVersion.description=Group/version of the snapshot node (e.g. `snapshot.storage.k8s.io/v1`).
	// +crd-enricher:raw:properties.kind.description=Kind of the snapshot node (e.g. `VolumeSnapshot`, `VirtualDiskSnapshot`).
	// +crd-enricher:raw:properties.kind.minLength=1
	// +crd-enricher:raw:properties.name.description=Name of the snapshot node.
	// +crd-enricher:raw:properties.name.minLength=1
	SnapshotRef *ObjectReference `json:"snapshotRef,omitempty"`
	// PopulateData only. Parameters of the transient scratch volume the imported bytes are
	// staged into before capture; it is destroyed after the durable VolumeSnapshotContent is
	// produced. Its StorageClass must be snapshot-capable (it references a VolumeSnapshotClass
	// via the `storage.deckhouse.io/volumesnapshotclass` annotation).
	// +optional
	StorageParams *StorageParamsSpec `json:"storageParams,omitempty"`

	// The PVC template types are shared with VolumeRestoreRequest, so this kind's descriptions and
	// validation of the template are set on this field rather than on the types.

	// CreatePVC only. PersistentVolumeClaim template fully describing the target PVC the
	// imported bytes are written into; the PVC is preserved after the import.
	// +optional
	// +crd-enricher:raw:properties.metadata.description=PersistentVolumeClaim metadata.
	// +crd-enricher:raw:properties.metadata.properties.name.description=PersistentVolumeClaim name.
	// +crd-enricher:raw:properties.metadata.properties.labels.description=PersistentVolumeClaim labels.
	// +crd-enricher:raw:properties.metadata.properties.annotations.description=PersistentVolumeClaim annotations.
	// +crd-enricher:raw:properties.spec.description=PersistentVolumeClaim specification.
	// +crd-enricher:raw:properties.spec.properties.accessModes.description=Desired access modes for the volume.
	// +crd-enricher:raw:properties.spec.properties.accessModes.items.enum=[ReadWriteOnce, ReadOnlyMany, ReadWriteMany, ReadWriteOncePod]
	// +crd-enricher:raw:properties.spec.properties.resources.description=Minimum resource requirements for the volume.
	// +crd-enricher:raw:properties.spec.properties.resources.properties.requests.description=Minimum amount of compute resources required.
	// +crd-enricher:raw:properties.spec.properties.storageClassName.description="Name of the StorageClass required by the PersistentVolumeClaim. When omitted,\nthe cluster's default StorageClass is used — also when it is marked default\nlater; the import waits until then. An empty string is not supported: the\nvolume has to be dynamically provisioned."
	// +crd-enricher:raw:properties.spec.properties.storageClassName.minLength=1
	// +crd-enricher:raw:properties.spec.properties.volumeMode.description=Volume mode required by the PersistentVolumeClaim.
	// +crd-enricher:raw:properties.spec.properties.volumeMode.enum=[Block, Filesystem]
	PvcTemplate *PersistentVolumeClaimTemplateSpec `json:"pvcTemplate,omitempty"`
}

// EffectiveMode returns the import mode, defaulting an empty value to CreatePVC (the CRD default) so
// controller logic never has to special-case the unset field.
func (s DataImportSpec) EffectiveMode() DataImportMode {
	if s.Mode == "" {
		return DataImportModeCreatePVC
	}
	return s.Mode
}

// StorageParamsSpec parameterizes the transient scratch volume used by a PopulateData import. The bytes
// are staged into a PVC shaped from these parameters and then captured into a durable
// VolumeSnapshotContent; the scratch PVC is destroyed afterwards. StorageClassName and Size are required;
// VolumeMode defaults to Filesystem when empty.
// +k8s:deepcopy-gen=true
type StorageParamsSpec struct {
	// The produced durable artifact is always a VolumeSnapshotContent, which is why the class has to be
	// snapshot-capable (its driver has a VolumeSnapshotClass).

	// StorageClass of the scratch PVC (must be snapshot-capable).
	// +kubebuilder:validation:MinLength=1
	StorageClassName string `json:"storageClassName"`
	// Requested size of the scratch PVC (a Kubernetes quantity, e.g. `10Gi`).
	// +kubebuilder:validation:MinLength=1
	Size string `json:"size"`
	// Volume mode of the scratch PVC. Defaults to `Filesystem` when omitted.
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
