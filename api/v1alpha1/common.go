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
