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

	// Data carries the durable cluster-scoped data artifact produced by a DataImport under a nested
	// data.artifact (VolumeSnapshotContent or PersistentVolume). It is written once the backing
	// VolumeCaptureRequest completes; the state-snapshotter import orchestrator reads data.artifact to
	// populate SnapshotContent.status.data.artifact. Empty for DataExport.
	// +optional
	Data *DataExportImportData `json:"data,omitempty"`
}

// DataExportImportData is the self-contained captured-data block on a DataImport status. It nests the
// durable artifact under data.artifact (symmetric with SnapshotContent.status.data and
// VolumeCaptureRequest.status.data).
// +k8s:deepcopy-gen=true
type DataExportImportData struct {
	// Artifact references the durable cluster-scoped data artifact (VolumeSnapshotContent or PersistentVolume).
	// +optional
	Artifact *DataArtifactReference `json:"artifact,omitempty"`
}

// DataArtifactReference references a cluster-scoped durable data artifact (VolumeSnapshotContent or
// PersistentVolume) by its apiVersion/kind/name.
// +k8s:deepcopy-gen=true
type DataArtifactReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	// UID is the durable data artifact UID (for example the VolumeSnapshotContent UID). It makes the
	// artifact reference self-contained, symmetric with VolumeCaptureRequest's status.data.artifact.uid.
	// Optional: producers fill it best-effort (the artifact may be referenced before its UID is known).
	// +optional
	UID string `json:"uid,omitempty"`
}

type Statusable interface {
	GetStatus() *DataExportImportStatus
}
