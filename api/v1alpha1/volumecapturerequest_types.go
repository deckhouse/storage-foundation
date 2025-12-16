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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen=true
// VolumeCaptureRequestSpec defines the desired state of VolumeCaptureRequest
type VolumeCaptureRequestSpec struct {
	// Mode specifies the capture mode: "Snapshot" or "Detach"
	// +kubebuilder:validation:Enum=Snapshot;Detach
	// +kubebuilder:validation:Required
	Mode string `json:"mode"`
	// PersistentVolumeClaimRef references the PVC to capture
	// Required for both Snapshot and Detach modes
	PersistentVolumeClaimRef *ObjectReference `json:"persistentVolumeClaimRef,omitempty"`
}

// +k8s:deepcopy-gen=true
// VolumeCaptureRequestStatus defines the observed state of VolumeCaptureRequest
type VolumeCaptureRequestStatus struct {
	// ObservedGeneration is the generation of the resource that was last processed
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// CompletionTimestamp is the time when the capture request completed
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`
	// Conditions represent the latest available observations of the resource's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// DataRef references the captured data (e.g., VolumeSnapshotContent name)
	DataRef *ObjectReference `json:"dataRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VolumeCaptureRequest is the Schema for the volumecapturerequests API
type VolumeCaptureRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VolumeCaptureRequestSpec   `json:"spec,omitempty"`
	Status            VolumeCaptureRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeCaptureRequestList contains a list of VolumeCaptureRequest
type VolumeCaptureRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeCaptureRequest `json:"items"`
}
