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
	"testing"

	crdv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestVSCModeIsolation verifies that VSCMode implementations are properly isolated.
// This test ensures that:
// - VSC-only mode works correctly
// - Legacy mode maintains upstream behavior
// - Modes don't interfere with each other

func TestVSCModeIsolation_LegacyMode(t *testing.T) {
	legacyMode := NewLegacyVSCMode()

	// Legacy VSC with VolumeSnapshotRef
	legacyContent := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "legacy-content",
			UID:  types.UID("legacy-content-uid"),
		},
		Spec: crdv1.VolumeSnapshotContentSpec{
			VolumeSnapshotRef: v1.ObjectReference{
				UID: types.UID("snapshot-uid"),
			},
		},
	}

	// Legacy mode should not recognize VSC-only
	if legacyMode.IsVSCOnly(legacyContent) {
		t.Error("Legacy mode should not recognize VSC with VolumeSnapshotRef as VSC-only")
	}

	// Legacy mode should use VolumeSnapshotRef.UID
	uid := legacyMode.SnapshotUID(legacyContent)
	if uid != "snapshot-uid" {
		t.Errorf("Legacy mode should use VolumeSnapshotRef.UID, got %s", uid)
	}
}

func TestVSCModeIsolation_VSCOnlyMode(t *testing.T) {
	vscOnlyMode := NewVSCOnlyMode()

	// VSC-only content without VolumeSnapshotRef
	vscOnlyContent := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vsc-only-content",
			UID:  types.UID("vsc-only-content-uid"),
		},
		Spec: crdv1.VolumeSnapshotContentSpec{
			VolumeSnapshotRef: v1.ObjectReference{}, // Empty
		},
	}

	// VSC-only mode should recognize empty VolumeSnapshotRef
	if !vscOnlyMode.IsVSCOnly(vscOnlyContent) {
		t.Error("VSC-only mode should recognize VSC without VolumeSnapshotRef")
	}

	// VSC-only mode should use VSC.UID
	uid := vscOnlyMode.SnapshotUID(vscOnlyContent)
	if uid != "vsc-only-content-uid" {
		t.Errorf("VSC-only mode should use VSC.UID, got %s", uid)
	}
}

func TestVSCModeIsolation_VSCOnlyBackwardCompatibility(t *testing.T) {
	vscOnlyMode := NewVSCOnlyMode()

	// VSC with VolumeSnapshotRef (legacy) should still work in VSC-only mode
	legacyContent := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "legacy-content",
			UID:  types.UID("legacy-content-uid"),
		},
		Spec: crdv1.VolumeSnapshotContentSpec{
			VolumeSnapshotRef: v1.ObjectReference{
				UID: types.UID("snapshot-uid"),
			},
		},
	}

	// VSC-only mode should not recognize legacy VSC as VSC-only
	if vscOnlyMode.IsVSCOnly(legacyContent) {
		t.Error("VSC-only mode should not recognize VSC with VolumeSnapshotRef as VSC-only")
	}

	// But should still use VolumeSnapshotRef.UID for backward compatibility
	uid := vscOnlyMode.SnapshotUID(legacyContent)
	if uid != "snapshot-uid" {
		t.Errorf("VSC-only mode should use VolumeSnapshotRef.UID for backward compatibility, got %s", uid)
	}
}

func TestVSCModeIsolation_ShouldCreateSnapshot(t *testing.T) {
	vscOnlyMode := NewVSCOnlyMode()
	legacyMode := NewLegacyVSCMode()

	volumeHandle := "volume-handle-1"

	// VSC-only content without VolumeSnapshotRef
	vscOnlyContent := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vsc-only-content",
			UID:  types.UID("vsc-only-content-uid"),
		},
		Spec: crdv1.VolumeSnapshotContentSpec{
			Source: crdv1.VolumeSnapshotContentSource{
				VolumeHandle: &volumeHandle,
			},
			VolumeSnapshotRef: v1.ObjectReference{}, // Empty
		},
		Status: nil,
	}

	// VSC-only mode should allow CreateSnapshot
	if !vscOnlyMode.ShouldCreateSnapshot(vscOnlyContent) {
		t.Error("VSC-only mode should allow CreateSnapshot for VSC without VolumeSnapshotRef")
	}

	// Legacy mode should NOT allow CreateSnapshot (requires VolumeSnapshotRef.UID)
	if legacyMode.ShouldCreateSnapshot(vscOnlyContent) {
		t.Error("Legacy mode should NOT allow CreateSnapshot for VSC without VolumeSnapshotRef.UID")
	}

	// Legacy VSC with VolumeSnapshotRef
	legacyContent := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "legacy-content",
			UID:  types.UID("legacy-content-uid"),
		},
		Spec: crdv1.VolumeSnapshotContentSpec{
			Source: crdv1.VolumeSnapshotContentSource{
				VolumeHandle: &volumeHandle,
			},
			VolumeSnapshotRef: v1.ObjectReference{
				UID: types.UID("snapshot-uid"),
			},
		},
		Status: nil,
	}

	// Both modes should allow CreateSnapshot for legacy VSC
	if !legacyMode.ShouldCreateSnapshot(legacyContent) {
		t.Error("Legacy mode should allow CreateSnapshot for VSC with VolumeSnapshotRef")
	}

	if !vscOnlyMode.ShouldCreateSnapshot(legacyContent) {
		t.Error("VSC-only mode should allow CreateSnapshot for legacy VSC (backward compatibility)")
	}
}
