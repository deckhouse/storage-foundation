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

	// DataArtifactRef references the durable cluster-scoped data artifact produced by a DataImport
	// (VolumeSnapshotContent or PersistentVolume). It is written once the backing VolumeCaptureRequest
	// completes; the state-snapshotter import orchestrator reads it to populate SnapshotContent.dataRef.
	// Empty for DataExport.
	DataArtifactRef *DataArtifactReference `json:"dataArtifactRef,omitempty"`
}

// DataArtifactReference references a cluster-scoped durable data artifact (VolumeSnapshotContent or
// PersistentVolume) by its apiVersion/kind/name.
// +k8s:deepcopy-gen=true
type DataArtifactReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

type Statusable interface {
	GetStatus() *DataExportImportStatus
}
