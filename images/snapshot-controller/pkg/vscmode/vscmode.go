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
)

// VSCMode defines the interface for VolumeSnapshotContent mode handling.
// This abstraction allows downstream extensions (like VSC-only mode) to be
// isolated from upstream snapshot-controller logic, making rebases easier.
type VSCMode interface {
	// IsVSCOnly returns true if the VolumeSnapshotContent is in VSC-only mode
	// (i.e., VolumeSnapshotRef is completely empty, no VolumeSnapshot involved).
	IsVSCOnly(content *crdv1.VolumeSnapshotContent) bool

	// SnapshotUID returns the UID to use for snapshot name generation.
	// This UID is used to create a deterministic snapshot name that CSI drivers can use
	// to identify and manage snapshots consistently.
	//
	// Upstream semantics (legacy mode):
	//   - Returns VolumeSnapshotRef.UID (the UID of the bound VolumeSnapshot object)
	//   - This ensures snapshot names are tied to VolumeSnapshot lifecycle
	//
	// Downstream extension (VSC-only mode):
	//   - Returns VolumeSnapshotContent.UID when VolumeSnapshotRef is empty
	//   - This allows VSC-only snapshots to work without VolumeSnapshot objects
	//   - For backward compatibility, if VolumeSnapshotRef.UID is set, it's used instead
	//
	// The returned UID is used by makeSnapshotName() to generate CSI snapshot names
	// in the format: "{prefix}-{snapshotUID}" (with optional truncation).
	SnapshotUID(content *crdv1.VolumeSnapshotContent) string

	// ShouldCreateSnapshot determines if CreateSnapshot should be called.
	// This encapsulates the logic for when to create snapshots based on the mode.
	ShouldCreateSnapshot(content *crdv1.VolumeSnapshotContent) bool

	// ShouldDeleteSnapshot determines if DeleteSnapshot should be called.
	// This encapsulates deletion logic based on the mode and content state.
	ShouldDeleteSnapshot(content *crdv1.VolumeSnapshotContent) bool

	// ShouldSkipCreateForInProgress returns true if CreateSnapshot should be skipped
	// because it's already in progress (annotation set but not completed).
	// This handles async CreateSnapshot scenarios where CSI drivers perform creation asynchronously.
	ShouldSkipCreateForInProgress(content *crdv1.VolumeSnapshotContent) bool

	// ShouldSkip returns true if the VolumeSnapshotContent should be skipped entirely
	// (no CreateSnapshot, no status check). This is used for VSC-only content that
	// lacks both VolumeHandle and SnapshotHandle.
	ShouldSkip(content *crdv1.VolumeSnapshotContent) bool

	// IsValidContentForReconcile returns true if the VolumeSnapshotContent should be
	// added to the controller's work queue for reconciliation.
	// This is used by isDriverMatch to determine if content should be processed.
	// Upstream logic filters out contents without any source (VolumeHandle or SnapshotHandle).
	// Downstream (VSC-only) needs to see such objects in sync for proper handling.
	IsValidContentForReconcile(content *crdv1.VolumeSnapshotContent) bool
}
