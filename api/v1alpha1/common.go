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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +k8s:deepcopy-gen=true
type DataExportImportStatus struct {
	Url             string             `json:"url"`
	CA              string             `json:"ca,omitempty"`
	PublicURL       string             `json:"publicURL"`
	AccessTimestamp metav1.Time        `json:"accessTimestamp"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
	VolumeMode      string             `json:"volumeMode,omitempty"`

	// Phase is the coarse-grained lifecycle state of the execution object, written EXCLUSIVELY by the
	// data-manager controller (never by the server pod). It mirrors the VMOP status model:
	// DataImport progresses Pending -> Ready -> Completed | Expired | Failed;
	// DataExport progresses Pending -> Ready -> Expired | Failed (it has no Completed phase).
	// Terminating denotes DeletionTimestamp != nil and is a transient state, not an outcome.
	// The catalog of allowed values lives in the common module (common.Phase*); the CRD schema pins the
	// per-kind enum (DataImport vs DataExport differ). Empty until the controller first reconciles.
	// +optional
	Phase string `json:"phase,omitempty"`

	// CompletionTimestamp is the time the object reached a terminal phase (Completed | Expired | Failed).
	// The controller sets it exactly once, when the phase first becomes terminal; the garbage collector
	// measures the object's retention age from this timestamp (not from creationTimestamp, because a
	// transfer may run for hours before it finishes). Nil while the object is non-terminal.
	// +optional
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`

	// ServerState is the raw progress signal reported by the exporter/importer server pod. The pod is the
	// ONLY writer of this field; the controller reads it and derives phase and conditions from it.
	// DataImport reports Ready | Finished | IdleExpired; DataExport reports Ready | IdleExpired
	// (it never produces an artifact, so it has no Finished state). The catalog of allowed values lives
	// in the common module (common.ServerState*). Empty until the server pod first reports.
	// +optional
	ServerState string `json:"serverState,omitempty"`

	// Data carries the durable cluster-scoped data artifact produced by a DataImport under a nested
	// data.artifactRef (VolumeSnapshotContent or PersistentVolume). It is written once the backing
	// VolumeCaptureRequest completes; the state-snapshotter import orchestrator reads data.artifactRef to
	// populate SnapshotContent.status.data.artifactRef. Empty for DataExport.
	// +optional
	Data *DataExportImportData `json:"data,omitempty"`

	// CleanupReason is a non-empty internal discriminator meaning: a managed resource was lost or
	// replaced, and this object MUST run failure-driven recovery before it may become terminal. The
	// controller sets it on the pass that detects the loss (that pass performs no mutations) and clears
	// it in the same status write that stamps the terminal phase, so a persisted phase=Failed always
	// means the mandatory recovery already finished. The catalog of allowed values lives in the common
	// module (common.CleanupReason*). Only DataExport writes it today.
	// +optional
	CleanupReason string `json:"cleanupReason,omitempty"`

	// Recovery pins the identity of the objects this transfer temporarily took over, so the controller
	// can undo the takeover after a restart or after the taken-over child is deleted. Written once
	// during provisioning, before the first mutation of a user-owned object. Empty while nothing is
	// taken over.
	// +optional
	Recovery *RecoveryStatus `json:"recovery,omitempty"`
}

// RecoveryStatus is the durable identity a controller needs to undo a temporary takeover of a user
// volume. Matching on namespace/name alone is not sufficient: a recreated object reuses the name but
// gets a fresh UID, so every takeover check compares UIDs. Unset fields mean "nothing taken over yet".
// +k8s:deepcopy-gen=true
type RecoveryStatus struct {
	// SourcePVCUID is the UID of the user's PersistentVolumeClaim that the transfer took the volume
	// from. Recovery refuses to rebind a PV to a same-named claim whose UID differs.
	// +optional
	SourcePVCUID string `json:"sourcePVCUID,omitempty"`

	// ExportPVCUID is the UID of the controller-owned claim the PV was temporarily bound to. It
	// distinguishes "our claim is gone" from "a foreign claim now holds our name".
	// +optional
	ExportPVCUID string `json:"exportPVCUID,omitempty"`

	// PVName is the name of the taken-over PersistentVolume.
	// +optional
	PVName string `json:"pvName,omitempty"`

	// PVUID is the UID of the taken-over PersistentVolume. It is kept here rather than on the PV
	// itself, where it would be read from the very object whose identity it is meant to prove.
	// +optional
	PVUID string `json:"pvUID,omitempty"`
}

// DataExportImportData is the self-contained captured-data block on a DataImport status. It nests the
// durable artifact under data.artifactRef (symmetric with SnapshotContent.status.data and
// VolumeCaptureRequest.status.data).
// +k8s:deepcopy-gen=true
type DataExportImportData struct {
	// ArtifactRef references the durable cluster-scoped data artifact (VolumeSnapshotContent or PersistentVolume).
	// +optional
	ArtifactRef *DataArtifactReference `json:"artifactRef,omitempty"`
}

// DataArtifactReference references a cluster-scoped durable data artifact (VolumeSnapshotContent or
// PersistentVolume) by its apiVersion/kind/name.
// +k8s:deepcopy-gen=true
type DataArtifactReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	// UID is the durable data artifact UID (for example the VolumeSnapshotContent UID). It makes the
	// artifact reference self-contained, symmetric with VolumeCaptureRequest's status.data.artifactRef.uid.
	// Optional: producers fill it best-effort (the artifact may be referenced before its UID is known).
	// +optional
	UID string `json:"uid,omitempty"`
}

type Statusable interface {
	GetStatus() *DataExportImportStatus
}
