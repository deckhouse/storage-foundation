/*
Copyright 2026 Flant JSC

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

// Package v1 defines a Deckhouse-owned client-side Go type for the CSI snapshot.storage.k8s.io/v1
// VolumeSnapshot, EXTENDED with the state-snapshotter domain-capture protocol status fields
// (captureState/childrenSnapshotRefs/sourceRef/conditions — see design §11.1/§11.3).
//
// Rationale (design §11.3, wave8 Block 3c): the storage-foundation VolumeSnapshot domain reconciler needs
// TYPED access to the protocol status fields the core writes/reads, but the CRD is the CSI VolumeSnapshot
// (owned by the external-snapshotter fork). The fork's own client module tracks a different Kubernetes API
// version line, so importing it into images/controller would force a version skew. Instead this package
// defines a minimal, Deckhouse-owned Go type for the SAME GVK (snapshot.storage.k8s.io/v1, kind
// VolumeSnapshot) that:
//
//   - models ONLY the fields the domain reconciler reads (spec.source.*) or the SDK writes/reads
//     (status.captureState / childrenSnapshotRefs / sourceRef / conditions), reusing the canonical
//     state-snapshotter API types so there is a single source of truth for the wire shape; and
//   - is registered in a DEDICATED scheme (never the manager's default scheme, which already maps this GVK
//     to the upstream external-snapshotter type) used by a dedicated, cache-less client.
//
// Round-trip safety: all reads/writes go through JSON merge patches on the /status subresource. Because a
// merge patch only carries the modeled fields that actually changed, CSI-native status fields this type
// does NOT model (readyToUse, boundVolumeSnapshotContentName, creationTime, restoreSize, error, the fork's
// boundSnapshotContentName, …) are neither serialized nor diffed and are therefore preserved untouched by
// the API server.
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

const (
	// GroupName is the CSI snapshot API group. The extended type deliberately shares the upstream GVK so a
	// single CRD (the CSI VolumeSnapshot, extended in crds/) backs both the upstream client and this type.
	GroupName = "snapshot.storage.k8s.io"
	// Version is the stable CSI VolumeSnapshot version that carries the domain protocol fields (v1beta1 is
	// legacy and is NEVER treated as a domain object).
	Version = "v1"
	// Kind is the CSI VolumeSnapshot kind.
	Kind = "VolumeSnapshot"
)

// SchemeGroupVersion is the GroupVersion this package registers its type under.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// GroupVersionKind is the fully-qualified GVK of the extended VolumeSnapshot.
var GroupVersionKind = SchemeGroupVersion.WithKind(Kind)

var (
	// SchemeBuilder registers the extended VolumeSnapshot type into a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the extended VolumeSnapshot type to a scheme. It MUST be used only on a dedicated
	// scheme, never the manager default scheme (which registers the upstream external-snapshotter type for
	// the same GVK — registering both collides).
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&VolumeSnapshot{},
		&VolumeSnapshotList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// VolumeSnapshot is the Deckhouse-owned, domain-extended view of a CSI VolumeSnapshot. It is a partial
// projection: only the spec/status fields the state-snapshotter domain protocol touches are modeled.
type VolumeSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeSnapshotSpec   `json:"spec"`
	Status VolumeSnapshotStatus `json:"status,omitempty"`
}

// VolumeSnapshotList is the list type for VolumeSnapshot.
type VolumeSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeSnapshot `json:"items"`
}

// VolumeSnapshotSpec models the subset of the CSI VolumeSnapshot spec the domain reconciler reads: the
// mode discriminator plus the snapshot source (PVC name vs pre-provisioned content). It is never
// written by the domain reconciler.
type VolumeSnapshotSpec struct {
	// Mode selects how this VolumeSnapshot obtains its content (Capture | Import), immutable once set;
	// default Capture (fork extension: parity with spec.mode on every other snapshot kind). Import
	// disables capture planning — the data artifact comes from a DataImport and spec.source stays empty.
	// +optional
	Mode string `json:"mode,omitempty"`
	// Source identifies where the snapshot comes from.
	Source VolumeSnapshotSource `json:"source"`
	// VolumeSnapshotClassName is the CSI snapshot class (read-only here; carried for completeness).
	// +optional
	VolumeSnapshotClassName *string `json:"volumeSnapshotClassName,omitempty"`
}

// VolumeSnapshotModeImport is the spec.mode value marking an import-mode VolumeSnapshot (fork
// extension). String-typed locally to keep this package free of a state-snapshotter api dependency for
// one constant; the value matches storagev1alpha1.SnapshotModeImport verbatim.
const VolumeSnapshotModeImport = "Import"

// VolumeSnapshotSource is the CSI snapshot source. Exactly one of PersistentVolumeClaimName /
// VolumeSnapshotContentName is set on a stock CSI VolumeSnapshot. Fork: source is required only when
// spec.mode != Import (an import snapshot omits it; this non-pointer field then decodes as the zero
// struct); an EMPTY source is the restore intent (volumeSnapshotContentName is one-shot set later).
type VolumeSnapshotSource struct {
	// PersistentVolumeClaimName names the source PVC (dynamic snapshot). When set, this VolumeSnapshot is a
	// data-leaf domain snapshot: its data leg is the CSI VolumeSnapshotContent and its manifest leg captures
	// the PVC object.
	// +optional
	PersistentVolumeClaimName *string `json:"persistentVolumeClaimName,omitempty"`
	// VolumeSnapshotContentName names a pre-provisioned VolumeSnapshotContent (static binding). Such a
	// snapshot has no live PVC source, so the domain reconciler skips it (no capture planning).
	// +optional
	VolumeSnapshotContentName *string `json:"volumeSnapshotContentName,omitempty"`
}

// VolumeSnapshotStatus models the domain-protocol subset of the CSI VolumeSnapshot status. CSI-native
// fields (readyToUse, boundVolumeSnapshotContentName, creationTime, restoreSize, error) and the fork's
// boundSnapshotContentName are intentionally NOT modeled: they are preserved by the API server because
// merge patches never carry unmodeled fields. Every field below mirrors the canonical state-snapshotter
// domain snapshot status contract (see api/storage/v1alpha1 and the reference demo snapshot).
type VolumeSnapshotStatus struct {
	// CaptureState is the umbrella for internal capture signals: commonController (core-written leg
	// latches) and domainSpecificController (domain-written planning refs/phase, via the SDK).
	// +optional
	CaptureState *storagev1alpha1.CaptureStateStatus `json:"captureState,omitempty"`
	// ChildrenSnapshotRefs is the top-level set of child snapshot refs. A VolumeSnapshot is a data leaf, so
	// this is always empty; it is modeled only because the SDK adapter reads/writes it uniformly.
	// +optional
	ChildrenSnapshotRefs []storagev1alpha1.SnapshotChildRef `json:"childrenSnapshotRefs,omitempty"`
	// SourceRef is the full reference to the captured live source (the PVC), published by the SDK
	// (PublishSnapshotSource) for import-mode recreation.
	// +optional
	SourceRef *storagev1alpha1.SnapshotSourceObjectRef `json:"sourceRef,omitempty"`
	// Conditions carries the single user-facing Ready condition, written by the core. The domain reconciler
	// only READS it (the failure channel: a terminal Ready reason drives CoreCaptureOutcome=Failed).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
