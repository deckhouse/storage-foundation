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

// LegacyVSCMode implements upstream snapshot-controller behavior.
// This mode requires VolumeSnapshotRef to be set and uses VolumeSnapshotRef.UID for snapshot naming.
type LegacyVSCMode struct{}

// NewLegacyVSCMode creates a new LegacyVSCMode instance.
func NewLegacyVSCMode() VSCMode {
	return &LegacyVSCMode{}
}

// IsVSCOnly returns false for legacy mode (always requires VolumeSnapshotRef).
func (m *LegacyVSCMode) IsVSCOnly(content *crdv1.VolumeSnapshotContent) bool {
	return false
}

// SnapshotUID returns VolumeSnapshotRef.UID for legacy mode.
// This matches upstream behavior where snapshot name is generated from VolumeSnapshot UID.
func (m *LegacyVSCMode) SnapshotUID(content *crdv1.VolumeSnapshotContent) string {
	return string(content.Spec.VolumeSnapshotRef.UID)
}

// ShouldCreateSnapshot returns true if CreateSnapshot should be called.
// For legacy mode, this follows upstream logic:
// - VolumeHandle must be present
// - Status must be nil OR SnapshotHandle must be nil (not already created)
// - Not a group snapshot member
func (m *LegacyVSCMode) ShouldCreateSnapshot(content *crdv1.VolumeSnapshotContent) bool {
	// Legacy mode requires VolumeSnapshotRef.UID to be set
	if content.Spec.VolumeSnapshotRef.UID == "" {
		return false
	}

	// Standard upstream checks
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
// For legacy mode, this follows upstream logic:
// - DeletionTimestamp must be set
// - AnnVolumeSnapshotBeingCreated must not be set (not in progress)
// - For pre-provisioned: VolumeSnapshotRef.UID == "" means delete immediately
// - AnnVolumeSnapshotBeingDeleted annotation triggers deletion
func (m *LegacyVSCMode) ShouldDeleteSnapshot(content *crdv1.VolumeSnapshotContent) bool {
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

	// Legacy: shouldDelete returns true if AnnVolumeSnapshotBeingDeleted annotation is set.
	if metav1.HasAnnotation(content.ObjectMeta, utils.AnnVolumeSnapshotBeingDeleted) {
		return true
	}

	return false
}
