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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen=true
// VolumeRestoreRequestSpec defines the desired state of VolumeRestoreRequest
type VolumeRestoreRequestSpec struct {
	// SourceRef references the source data to restore from (VolumeSnapshotContent or PersistentVolume)
	SourceRef ObjectReference `json:"sourceRef"`
	// TargetNamespace is the namespace where the restored PVC will be created
	TargetNamespace string `json:"targetNamespace"`
	// TargetPVCName is the name of the PVC to create
	TargetPVCName string `json:"targetPVCName"`
	// StorageClassName is the storage class to use for the restored PVC
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
	// VolumeMode specifies whether the restored volume is Block or Filesystem.
	// It is required: the executor builds CSI VolumeCapabilities from it and there is
	// no implicit default (mismatched mode breaks Block volumes).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Block;Filesystem
	VolumeMode corev1.PersistentVolumeMode `json:"volumeMode"`
	// FsType is the filesystem type for Filesystem volumes (optional, ignored for Block).
	// +optional
	FsType string `json:"fsType,omitempty"`
	// AccessModes is the list of access modes for the restored PVC
	// (optional, defaults to ReadWriteOnce). Enables parity with the PV-source restore
	// path (e.g. RWX). Invalid modes are rejected by admission via the Enum below.
	// +optional
	// +kubebuilder:validation:items:Enum=ReadWriteOnce;ReadOnlyMany;ReadWriteMany;ReadWriteOncePod
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
}

// +k8s:deepcopy-gen=true
// VolumeRestoreRequestStatus defines the observed state of VolumeRestoreRequest
type VolumeRestoreRequestStatus struct {
	// CompletionTimestamp is the time when the restore request completed
	CompletionTimestamp *metav1.Time `json:"completionTimestamp,omitempty"`
	// Conditions represent the latest available observations of the resource's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// TargetPVCRef references the created target PVC
	TargetPVCRef *ObjectReference `json:"targetPVCRef,omitempty"`
}

// +kubebuilder:object:root=true
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
