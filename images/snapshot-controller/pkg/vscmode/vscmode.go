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
	// For legacy mode: returns VolumeSnapshotRef.UID
	// For VSC-only mode: returns VolumeSnapshotContent.UID
	SnapshotUID(content *crdv1.VolumeSnapshotContent) string

	// ShouldCreateSnapshot determines if CreateSnapshot should be called.
	// This encapsulates the logic for when to create snapshots based on the mode.
	ShouldCreateSnapshot(content *crdv1.VolumeSnapshotContent) bool

	// ShouldDeleteSnapshot determines if DeleteSnapshot should be called.
	// This encapsulates deletion logic based on the mode and content state.
	ShouldDeleteSnapshot(content *crdv1.VolumeSnapshotContent) bool
}
