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
// VolumeRestoreRequestSpec defines the desired state of VolumeRestoreRequest.
//
// The suffix convention is authoritative: sourceRef (Ref) points at an object that already EXISTS;
// pvcTemplate (Template) is the spec of the PVC the restore CREATES. The external-provisioner
// provisions the PV from sourceRef and creates the PVC from pvcTemplate (name required), then binds
// them in the VRR namespace (restore is never cross-namespace).
// +kubebuilder:validation:XValidation:rule="size(self.pvcTemplate.metadata.name) > 0",message="pvcTemplate.metadata.name is required"
type VolumeRestoreRequestSpec struct {
	// SourceRef references the source data to restore from (VolumeSnapshotContent or PersistentVolume).
	SourceRef ObjectReference `json:"sourceRef"`
	// PvcTemplate is the spec of the PersistentVolumeClaim the restore creates and binds. It absorbs the
	// former root storageClassName/volumeMode/accessModes/size; the PVC name lives in
	// pvcTemplate.metadata.name (required). Namespace is implicit = the VRR namespace.
	PvcTemplate PersistentVolumeClaimTemplateSpec `json:"pvcTemplate"`
	// FsType is the filesystem type for Filesystem volumes (optional, ignored for Block). It is a restore
	// execution parameter read by the external-provisioner, not a PVC field, so it stays at spec root.
	// +optional
	FsType string `json:"fsType,omitempty"`
}

// +k8s:deepcopy-gen=true
// VolumeRestoreRequestStatus defines the observed state of VolumeRestoreRequest
type VolumeRestoreRequestStatus struct {
	// CompletionTimestamp is the time when the restore request completed
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`
	// Conditions represent the latest available observations of the resource's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// PvcRef references the created restore target PersistentVolumeClaim. Unlike spec, the namespace and
	// uid are populated here so the status is self-contained and consumers can detect recreation.
	PvcRef *ObjectReference `json:"pvcRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:metadata:labels=module=storage-foundation
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// VolumeRestoreRequest is the Schema for the volumerestorerequests API
type VolumeRestoreRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VolumeRestoreRequestSpec   `json:"spec,omitempty"`
	Status            VolumeRestoreRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeRestoreRequestList contains a list of VolumeRestoreRequest
type VolumeRestoreRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeRestoreRequest `json:"items"`
}
