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

const (
	VolumeCaptureModeSnapshot = "Snapshot"
	VolumeCaptureModeDetach   = "Detach"
)

// +k8s:deepcopy-gen=true
// VolumeCaptureTarget identifies a PVC (or future volume target) to capture.
type VolumeCaptureTarget struct {
	// UID is the map key for spec.targets (PersistentVolumeClaim UID).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	UID string `json:"uid"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// +k8s:deepcopy-gen=true
// VolumeDataArtifactRef points to a durable data artifact produced by the data path.
// It MUST reference a final artifact such as VolumeSnapshotContent or PersistentVolume.
// It MUST NOT reference execution requests such as VolumeCaptureRequest.
type VolumeDataArtifactRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// +k8s:deepcopy-gen=true
// VolumeDataBinding associates a capture target with its durable data artifact on one VCR.
type VolumeDataBinding struct {
	// TargetUID is the map key for status.dataRefs (matches spec.targets[].uid).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	TargetUID string `json:"targetUID"`

	// Target identifies the PVC target captured in this binding.
	Target VolumeCaptureTarget `json:"target"`

	// Artifact references the cluster-scoped durable data artifact.
	Artifact VolumeDataArtifactRef `json:"artifact"`
}

// +k8s:deepcopy-gen=true
// VolumeCaptureRequestSpec defines the desired state of VolumeCaptureRequest
type VolumeCaptureRequestSpec struct {
	// Mode specifies the capture mode: "Snapshot" or "Detach"
	// +kubebuilder:validation:Enum=Snapshot;Detach
	// +kubebuilder:validation:Required
	Mode string `json:"mode"`

	// Targets lists PVC/volume targets to capture in this request (bulk, one VCR per logical snapshot node).
	// +listType=map
	// +listMapKey=uid
	// +optional
	Targets []VolumeCaptureTarget `json:"targets,omitempty"`
}

// +k8s:deepcopy-gen=true
// VolumeCaptureRequestStatus defines the observed state of VolumeCaptureRequest
type VolumeCaptureRequestStatus struct {
	// CompletionTimestamp is the time when the capture request completed
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`
	// Conditions represent the latest available observations of the resource's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// DataRefs lists per-target durable data artifacts (for example VolumeSnapshotContent).
	// +listType=map
	// +listMapKey=targetUID
	// +optional
	DataRefs []VolumeDataBinding `json:"dataRefs,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.spec.mode != 'Snapshot' || size(self.spec.targets) > 0",message="spec.targets must not be empty when mode is Snapshot"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// VolumeCaptureRequest is the Schema for the volumecapturerequests API
type VolumeCaptureRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeCaptureRequestSpec   `json:"spec,omitempty"`
	Status VolumeCaptureRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type VolumeCaptureRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeCaptureRequest `json:"items"`
}
