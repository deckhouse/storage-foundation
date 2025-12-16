/*
Copyright 2019 The Kubernetes Authors.

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

package vscmode

import (
	crdv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/kubernetes-csi/external-snapshotter/v8/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VSCOnlyMode implements VSC-only model behavior.
// In this mode, VolumeSnapshotContent can exist without VolumeSnapshot,
// and VolumeSnapshotRef is completely empty.
type VSCOnlyMode struct{}

// NewVSCOnlyMode creates a new VSCOnlyMode instance.
func NewVSCOnlyMode() VSCMode {
	return &VSCOnlyMode{}
}

// IsVSCOnly returns true if VolumeSnapshotRef is completely empty (VSC-only model).
func (m *VSCOnlyMode) IsVSCOnly(content *crdv1.VolumeSnapshotContent) bool {
	return content.Spec.VolumeSnapshotRef.UID == "" &&
		content.Spec.VolumeSnapshotRef.Name == "" &&
		content.Spec.VolumeSnapshotRef.Namespace == ""
}

// SnapshotUID returns VolumeSnapshotContent.UID for VSC-only mode.
// This allows VSC-only snapshots to work without VolumeSnapshot.
// NOTE: We use content.UID (not content.Name) to match legacy behavior where VolumeSnapshotRef.UID is used.
// content.UID is stable for the lifetime of the object, but changes if VSC is recreated.
func (m *VSCOnlyMode) SnapshotUID(content *crdv1.VolumeSnapshotContent) string {
	if content.Spec.VolumeSnapshotRef.UID != "" {
		// Legacy mode: VolumeSnapshotRef is set - use it for backward compatibility
		return string(content.Spec.VolumeSnapshotRef.UID)
	}
	// VSC-only mode: VolumeSnapshotRef is empty - use VSC.UID
	return string(content.UID)
}

// ShouldCreateSnapshot returns true if CreateSnapshot should be called.
// For VSC-only mode:
// - VolumeHandle must be present
// - Status must be nil OR SnapshotHandle must be nil (not already created)
// - Not a group snapshot member
// - Works with or without VolumeSnapshotRef
func (m *VSCOnlyMode) ShouldCreateSnapshot(content *crdv1.VolumeSnapshotContent) bool {
	if content.Spec.Source.VolumeHandle == nil {
		return false
	}

	// If Status exists and SnapshotHandle is set, snapshot was already created
	if content.Status != nil && content.Status.SnapshotHandle != nil {
		return false
	}

	_, groupSnapshotMember := content.Annotations[utils.VolumeGroupSnapshotHandleAnnotation]
	if groupSnapshotMember {
		return false
	}

	return true
}

// ShouldDeleteSnapshot returns true if DeleteSnapshot should be called.
// For VSC-only mode:
//   - DeletionTimestamp must be set
//   - AnnVolumeSnapshotBeingCreated must not be set (not in progress)
//   - AnnVolumeSnapshotBeingDeleted annotation triggers deletion (set by common-controller)
//   - If DeletionTimestamp is set and we have SnapshotHandle in status, proceed with deletion
//     (common-controller may not set the annotation for VSC-only, so we check status directly)
//
// Invariants enforced:
//   - Does not delete if DeletionTimestamp is nil
//   - Does not delete if CreateSnapshot is in progress (AnnVolumeSnapshotBeingCreated set)
//   - Does not delete if snapshot not yet created (Status.SnapshotHandle is nil)
//   - DeletionPolicy check is performed in sidecar-controller before calling DeleteSnapshot
func (m *VSCOnlyMode) ShouldDeleteSnapshot(content *crdv1.VolumeSnapshotContent) bool {
	if content.ObjectMeta.DeletionTimestamp == nil {
		return false
	}

	// Don't delete if CreateSnapshot is in progress
	if metav1.HasAnnotation(content.ObjectMeta, utils.AnnVolumeSnapshotBeingCreated) {
		return false
	}

	// Legacy: For pre-provisioned snapshots (SnapshotHandle in Source and VolumeSnapshotRef.UID == ""), delete immediately.
	if content.Spec.Source.SnapshotHandle != nil && content.Spec.VolumeSnapshotRef.UID == "" {
		return true
	}

	// VSC-only or legacy: shouldDelete returns true if AnnVolumeSnapshotBeingDeleted annotation is set.
	// This annotation is set by common-controller when deletion should proceed.
	if metav1.HasAnnotation(content.ObjectMeta, utils.AnnVolumeSnapshotBeingDeleted) {
		return true
	}

	// VSC-only model: If DeletionTimestamp is set and we have SnapshotHandle in status,
	// we can proceed with deletion. This covers VSC-only snapshots created by VCR.
	// Common-controller may not set the annotation for VSC-only, so we check status directly.
	if content.Status != nil && content.Status.SnapshotHandle != nil {
		// We have a snapshot to delete - proceed with deletion
		return true
	}

	return false
}
