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
	// UID is the captured PersistentVolumeClaim UID.
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
	// Namespace is intentionally empty in spec.target (the PVC always lives in the VCR namespace).
	// The captured PVC identity is carried by spec.target (immutable); status.data no longer duplicates it.
	// +optional
	Namespace string `json:"namespace,omitempty"`
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
	// UID is the durable data artifact UID (for example the VolumeSnapshotContent UID). It makes the
	// artifact reference self-contained, symmetric with target.uid. Optional: the artifact may be
	// referenced before its UID is known, so producers fill it best-effort.
	// +optional
	UID string `json:"uid,omitempty"`
}

// +k8s:deepcopy-gen=true
// VolumeDataBinding carries the durable data artifact produced for the VCR's captured target.
// The captured PVC identity is not duplicated here: it lives in spec.target (immutable), so status.data
// carries only the artifact.
type VolumeDataBinding struct {
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

	// Target is the single PVC/volume target to capture (one VCR per logical snapshot node, ≤1 volume).
	// +optional
	Target *VolumeCaptureTarget `json:"target,omitempty"`
}

// +k8s:deepcopy-gen=true
// VolumeCaptureRequestStatus defines the observed state of VolumeCaptureRequest
type VolumeCaptureRequestStatus struct {
	// CompletionTimestamp is the time when the capture request completed
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`
	// Conditions represent the latest available observations of the resource's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Data is the durable data artifact for the captured target (for example VolumeSnapshotContent).
	// The captured PVC identity comes from spec.target (immutable); data carries only the artifact.
	// +optional
	Data *VolumeDataBinding `json:"data,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:metadata:labels=module=storage-foundation
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.spec.mode != 'Snapshot' || has(self.spec.target)",message="spec.target is required when mode is Snapshot"
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
