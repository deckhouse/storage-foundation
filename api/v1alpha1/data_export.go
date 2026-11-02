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
	"crypto/x509"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The whole DataExport object is intentionally NOT immutable (it was once locked
// with the Deckhouse-only x-kubernetes-immutable extension): only spec.targetRef
// -- the export identity -- is locked after creation, by the CEL transition rule
// on DataExportSpec. The other spec fields (ttl, publish, publicIngress) may still
// change. This mirrors DataImport, which locks only spec.mode and spec.snapshotRef.
//
// The status is the DataExportImportStatus shared with DataImport; its
// DataExport-specific descriptions and enums are set here. status.data is
// DataImport's only and is not part of this kind's schema.

// Resource for exporting persistent volume files and blocks.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=de
// +kubebuilder:metadata:labels={"heritage=deckhouse","module=storage-foundation","backup.deckhouse.io/cluster-config=true"}
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.url"
// +kubebuilder:printcolumn:name="Public URL",type="string",JSONPath=".status.publicURL"
// +kubebuilder:printcolumn:name="TTL",type="string",JSONPath=".spec.ttl",priority=1
// +kubebuilder:printcolumn:name="Target Kind",type="string",JSONPath=".spec.targetRef.kind",priority=1
// +kubebuilder:printcolumn:name="Target Name",type="string",JSONPath=".spec.targetRef.name",priority=1
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase",description="The status of DataExport."
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +crd-enricher:crd:minimal=true
// +crd-enricher:crd:preserveUnknownFields=false
// +crd-enricher:raw:properties.status.description=Status information for DataExport.
// +crd-enricher:unset:properties.status.properties.data
// +crd-enricher:raw:properties.status.properties.url.description=Internal URL of the exporter pod (e.g., https://X.X.X.X:8085).
// +crd-enricher:raw:properties.status.properties.ca.description=Base64 encoded CA certificate for TLS connection to the exporter pod.
// +crd-enricher:raw:properties.status.properties.publicURL.description="Public URL for external access (for example, https://api.<PUBLIC_DOMAIN>/<NAMESPACE>/<TARGET_KIND>/<TARGET_NAME>/). `TARGET_KIND` is the short kind of the export target: pvc, vd, or snap; `TARGET_NAME` is the value of spec.targetRef.name."
// +crd-enricher:raw:properties.status.properties.accessTimestamp.description=Timestamp of the last access to the exported data.
// +crd-enricher:raw:properties.status.properties.volumeMode.description=Volume mode of the exported data.
// +crd-enricher:raw:properties.status.properties.phase.enum=[Pending, Ready, Expired, Failed, Terminating]
// +crd-enricher:raw:properties.status.properties.phase.description="Lifecycle state of the DataExport: Pending → Ready → Expired | Failed. `Terminating` denotes an object with a deletion timestamp and is a transient state, not an outcome. `Expired` is a normal terminal outcome when the idle-TTL window elapses."
// +crd-enricher:raw:properties.status.properties.completionTimestamp.description="Time the DataExport reached a terminal phase (Expired or Failed). Retention age is measured from this timestamp."
// +crd-enricher:raw:properties.status.properties.serverState.enum=[Ready, IdleExpired]
// +crd-enricher:raw:properties.status.properties.serverState.description="Exporter server state: `Ready` when the server is available and `IdleExpired` when the idle-TTL window has elapsed."
// +crd-enricher:raw:properties.status.properties.conditions.description=Array of conditions describing the current state of DataExport.
// +crd-enricher:unset:properties.status.properties.conditions.items.description
// +crd-enricher:raw:properties.status.properties.conditions.items.required=[type, status, reason, message, observedGeneration, lastTransitionTime]
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.type.description=Type of the condition.
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.type.enum=[Ready]
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.status.description=Status of the condition.
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.reason.description=Reason for the current condition status.
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.reason.enum=[Pending, ServerReady, ValidationFailed, TargetNotReady, TargetNotFound, PVConflict, DeploymentFailed, CleanupFailed, PublishFailed, Expired]
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.message.description=Message describing the condition.
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.lastTransitionTime.description=Last time the condition transitioned from one status to another.
// +crd-enricher:raw:properties.status.properties.conditions.items.properties.observedGeneration.description=Observed generation of the resource when the condition was last updated.
type DataExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec DataExportSpec `json:"spec"`
	// +optional
	Status DataExportImportStatus `json:"status"`
}

// +kubebuilder:object:root=true
type DataExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []DataExport `json:"items"`
}

// targetRef is the export identity (what is exported): set once at creation and
// never changed. The other spec fields (ttl, publish, publicIngress) may be
// updated freely. The rule compares the identity field by field and normalizes
// the optional group (absent == core group == ""): the controller round-trips
// targetRef through a Go struct whose group is `json:",omitempty"`, so a create
// that sent group:"" is written back with group omitted. A whole-object
// self.targetRef == oldSelf.targetRef would wrongly flag that serialization no-op
// as a mutation and reject the controller's own finalizer/status update, wedging
// reconciliation.

// +k8s:deepcopy-gen=true
// +kubebuilder:validation:XValidation:rule="self.targetRef.kind == oldSelf.targetRef.kind && self.targetRef.name == oldSelf.targetRef.name && (has(self.targetRef.group) ? self.targetRef.group : \"\") == (has(oldSelf.targetRef.group) ? oldSelf.targetRef.group : \"\")",message="spec.targetRef is immutable"
type DataExportSpec struct {
	// Time-to-live duration after the last user request. Acceptable values: <number>s, <number>m, <number>h. For example, 5m, 2h45m, or 24h.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?h)?([0-9]+(\.[0-9]+)?m)?([0-9]+s)?$`
	TTL string `json:"ttl"`
	// Enable external access to the exporter pod outside the cluster.
	// +optional
	Publish bool `json:"publish"`
	// Type of public ingress for external access.
	// +optional
	// +kubebuilder:validation:Enum=KubernetesAPI;ConsoleFrontend
	PublicIngress string `json:"publicIngress,omitempty"`
	// Reference to the target object for export. The target must be in the same namespace
	// as the DataExport. PersistentVolumeClaim and VirtualDisk objects can be exported
	// directly; snapshot resources can also be used as export targets. A cluster-scoped
	// VolumeSnapshotContent cannot be used as an export target.
	TargetRef DataExportTargetRefSpec `json:"targetRef"`
}

// DataExportTargetRefSpec references the export target by GroupKind + name (namespace is implicit = the
// DataExport's own namespace). The version is intentionally NOT pinned: the controller resolves the
// served version and the plural resource via the RESTMapper (RESTMapping on the {group, kind}). Snapshot
// targets (any registered snapshot CR: generic VolumeSnapshot, domain snapshot, ...) are resolved
// generically through the leaf's bound SnapshotContent.dataRef; the live PVC/VirtualDisk direct-export
// targets remain special-cased. A bare cluster-scoped VolumeSnapshotContent is rejected (cross-namespace
// exfiltration).
// +k8s:deepcopy-gen=true
type DataExportTargetRefSpec struct {
	// API group of the target object ("" / omitted for the core group, e.g.
	// for PersistentVolumeClaim).
	// +optional
	Group string `json:"group,omitempty"`
	// Kind of the target (e.g. PersistentVolumeClaim, VirtualDisk, VolumeSnapshot,
	// VirtualDiskSnapshot).
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// Name of the target object for export.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// func init() {
// 	SchemeBuilder.Register(&DataExport{}, &DataExportList{})
// }

type AuthData struct {
	AuthType   string
	Token      string
	Username   string
	Password   string
	ClientCert x509.Certificate
}

func (de *DataExport) GetStatus() *DataExportImportStatus {
	return &de.Status
}
