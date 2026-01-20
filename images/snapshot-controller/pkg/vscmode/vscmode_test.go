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
	"github.com/kubernetes-csi/external-snapshotter/v8/pkg/utils"
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

// TestVSCModeIsolation_ShouldSkipCreateForInProgress verifies that ShouldSkipCreateForInProgress
// correctly handles async CreateSnapshot scenarios.
func TestVSCModeIsolation_ShouldSkipCreateForInProgress(t *testing.T) {
	legacyMode := NewLegacyVSCMode()
	vscOnlyMode := NewVSCOnlyMode()

	volumeHandle := "volume-handle-1"
	True := true
	False := false

	tests := []struct {
		name           string
		mode           VSCMode
		content        *crdv1.VolumeSnapshotContent
		expectedResult bool
		description    string
	}{
		{
			name: "Legacy: No annotation - should not skip",
			mode: legacyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("legacy-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{UID: types.UID("snapshot-uid")},
					Source:            crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
			},
			expectedResult: false,
			description:    "No annotation set, should not skip",
		},
		{
			name: "Legacy: Annotation set, no status - should skip",
			mode: legacyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("legacy-content-uid"),
					Annotations: map[string]string{
						utils.AnnVolumeSnapshotBeingCreated: "yes",
					},
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{UID: types.UID("snapshot-uid")},
					Source:            crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
			},
			expectedResult: true,
			description:    "Annotation set but no status, CreateSnapshot in progress",
		},
		{
			name: "Legacy: Annotation set, SnapshotHandle exists but not ready - should skip",
			mode: legacyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("legacy-content-uid"),
					Annotations: map[string]string{
						utils.AnnVolumeSnapshotBeingCreated: "yes",
					},
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{UID: types.UID("snapshot-uid")},
					Source:            crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
				Status: &crdv1.VolumeSnapshotContentStatus{
					SnapshotHandle: toStringPointer("snapshot-handle"),
					ReadyToUse:     &False,
				},
			},
			expectedResult: true,
			description:    "Annotation set, SnapshotHandle exists but ReadyToUse is false",
		},
		{
			name: "Legacy: Annotation set, ready - should not skip",
			mode: legacyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("legacy-content-uid"),
					Annotations: map[string]string{
						utils.AnnVolumeSnapshotBeingCreated: "yes",
					},
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{UID: types.UID("snapshot-uid")},
					Source:            crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
				Status: &crdv1.VolumeSnapshotContentStatus{
					SnapshotHandle: toStringPointer("snapshot-handle"),
					ReadyToUse:     &True,
				},
			},
			expectedResult: false,
			description:    "Annotation set but content is ready, no need to skip",
		},
		{
			name: "VSC-only: No annotation - should not skip",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					Source: crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
			},
			expectedResult: false,
			description:    "No annotation set, should not skip",
		},
		{
			name: "VSC-only: Annotation set, no status - should skip",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
					Annotations: map[string]string{
						utils.AnnVolumeSnapshotBeingCreated: "yes",
					},
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					Source: crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
			},
			expectedResult: true,
			description:    "Annotation set but no status, CreateSnapshot in progress",
		},
		{
			name: "VSC-only: Annotation set, ready - should not skip",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
					Annotations: map[string]string{
						utils.AnnVolumeSnapshotBeingCreated: "yes",
					},
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					Source: crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
				Status: &crdv1.VolumeSnapshotContentStatus{
					SnapshotHandle: toStringPointer("snapshot-handle"),
					ReadyToUse:     &True,
				},
			},
			expectedResult: false,
			description:    "Annotation set but content is ready, no need to skip",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := test.mode.ShouldSkipCreateForInProgress(test.content)
			if result != test.expectedResult {
				t.Errorf("Expected %v, got %v: %s", test.expectedResult, result, test.description)
			}
		})
	}
}

// TestVSCModeIsolation_ShouldSkip verifies that ShouldSkip correctly identifies
// content that should be skipped entirely.
func TestVSCModeIsolation_ShouldSkip(t *testing.T) {
	legacyMode := NewLegacyVSCMode()
	vscOnlyMode := NewVSCOnlyMode()

	volumeHandle := "volume-handle-1"
	snapshotHandle := "snapshot-handle-1"

	tests := []struct {
		name           string
		mode           VSCMode
		content        *crdv1.VolumeSnapshotContent
		expectedResult bool
		description    string
	}{
		{
			name: "Legacy: Never skip",
			mode: legacyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("legacy-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{UID: types.UID("snapshot-uid")},
					Source:            crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
			},
			expectedResult: false,
			description:    "Legacy mode never skips content processing",
		},
		{
			name: "Legacy: No VolumeHandle - still don't skip",
			mode: legacyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("legacy-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{UID: types.UID("snapshot-uid")},
				},
			},
			expectedResult: false,
			description:    "Legacy mode never skips, even without VolumeHandle",
		},
		{
			name: "VSC-only: Has VolumeHandle - don't skip",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					Source: crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
			},
			expectedResult: false,
			description:    "VSC-only with VolumeHandle should not be skipped",
		},
		{
			name: "VSC-only: No VolumeHandle but has SnapshotHandle - don't skip",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{},
				Status: &crdv1.VolumeSnapshotContentStatus{
					SnapshotHandle: &snapshotHandle,
				},
			},
			expectedResult: false,
			description:    "VSC-only with SnapshotHandle should not be skipped (pre-provisioned)",
		},
		{
			name: "VSC-only: No VolumeHandle and no SnapshotHandle - skip",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{},
			},
			expectedResult: true,
			description:    "VSC-only without VolumeHandle and SnapshotHandle should be skipped",
		},
		{
			name: "VSC-only: No VolumeHandle, no status - skip",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{},
			},
			expectedResult: true,
			description:    "VSC-only without VolumeHandle and without status should be skipped",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := test.mode.ShouldSkip(test.content)
			if result != test.expectedResult {
				t.Errorf("Expected %v, got %v: %s", test.expectedResult, result, test.description)
			}
		})
	}
}

// TestVSCMode_SnapshotUID_Legacy verifies that SnapshotUID returns VolumeSnapshotRef.UID
// for legacy mode, matching upstream behavior.
func TestVSCMode_SnapshotUID_Legacy(t *testing.T) {
	legacyMode := NewLegacyVSCMode()

	tests := []struct {
		name        string
		content     *crdv1.VolumeSnapshotContent
		expectedUID string
		description string
	}{
		{
			name: "Legacy: VolumeSnapshotRef.UID is used",
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{
						UID: types.UID("snapshot-uid"),
					},
				},
			},
			expectedUID: "snapshot-uid",
			description: "Legacy mode should use VolumeSnapshotRef.UID for snapshot name generation",
		},
		{
			name: "Legacy: Empty VolumeSnapshotRef.UID returns empty string",
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{},
				},
			},
			expectedUID: "",
			description: "Legacy mode returns empty string when VolumeSnapshotRef.UID is not set",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			uid := legacyMode.SnapshotUID(test.content)
			if uid != test.expectedUID {
				t.Errorf("Expected UID %q, got %q: %s", test.expectedUID, uid, test.description)
			}
		})
	}
}

// TestVSCMode_SnapshotUID_VSCOnly verifies that SnapshotUID returns content.UID
// for VSC-only mode, or VolumeSnapshotRef.UID for backward compatibility.
func TestVSCMode_SnapshotUID_VSCOnly(t *testing.T) {
	vscOnlyMode := NewVSCOnlyMode()

	tests := []struct {
		name        string
		content     *crdv1.VolumeSnapshotContent
		expectedUID string
		description string
	}{
		{
			name: "VSC-only: content.UID is used when VolumeSnapshotRef is empty",
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{}, // Empty
				},
			},
			expectedUID: "vsc-content-uid",
			description: "VSC-only mode should use content.UID when VolumeSnapshotRef is empty",
		},
		{
			name: "VSC-only: VolumeSnapshotRef.UID is used for backward compatibility",
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{
						UID: types.UID("snapshot-uid"),
					},
				},
			},
			expectedUID: "snapshot-uid",
			description: "VSC-only mode should use VolumeSnapshotRef.UID for backward compatibility when set",
		},
		{
			name: "VSC-only: Empty VolumeSnapshotRef and empty UID returns empty string",
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  "", // Empty UID
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{}, // Empty
				},
			},
			expectedUID: "",
			description: "VSC-only mode returns empty string when both VolumeSnapshotRef and content.UID are empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			uid := vscOnlyMode.SnapshotUID(test.content)
			if uid != test.expectedUID {
				t.Errorf("Expected UID %q, got %q: %s", test.expectedUID, uid, test.description)
			}
		})
	}
}

// TestVSCMode_IsValidContentForReconcile verifies that IsValidContentForReconcile
// correctly determines which content should be added to the controller's work queue.
func TestVSCMode_IsValidContentForReconcile(t *testing.T) {
	legacyMode := NewLegacyVSCMode()
	vscOnlyMode := NewVSCOnlyMode()

	volumeHandle := "volume-handle-1"
	snapshotHandle := "snapshot-handle-1"

	tests := []struct {
		name           string
		mode           VSCMode
		content        *crdv1.VolumeSnapshotContent
		expectedResult bool
		description    string
	}{
		{
			name: "Legacy: Has VolumeHandle - valid",
			mode: legacyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("legacy-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{UID: types.UID("snapshot-uid")},
					Source:            crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
			},
			expectedResult: true,
			description:    "Legacy mode allows content with VolumeHandle",
		},
		{
			name: "Legacy: Has SnapshotHandle - valid",
			mode: legacyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("legacy-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{UID: types.UID("snapshot-uid")},
					Source:            crdv1.VolumeSnapshotContentSource{SnapshotHandle: &snapshotHandle},
				},
			},
			expectedResult: true,
			description:    "Legacy mode allows content with SnapshotHandle (pre-provisioned)",
		},
		{
			name: "Legacy: No VolumeHandle and no SnapshotHandle - invalid",
			mode: legacyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-content",
					UID:  types.UID("legacy-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					VolumeSnapshotRef: v1.ObjectReference{UID: types.UID("snapshot-uid")},
					Source:            crdv1.VolumeSnapshotContentSource{},
				},
			},
			expectedResult: false,
			description:    "Legacy mode filters out content without VolumeHandle or SnapshotHandle (upstream behavior)",
		},
		{
			name: "VSC-only: Has VolumeHandle - valid",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					Source: crdv1.VolumeSnapshotContentSource{VolumeHandle: &volumeHandle},
				},
			},
			expectedResult: true,
			description:    "VSC-only mode allows content with VolumeHandle",
		},
		{
			name: "VSC-only: Has SnapshotHandle - valid",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					Source: crdv1.VolumeSnapshotContentSource{SnapshotHandle: &snapshotHandle},
				},
			},
			expectedResult: true,
			description:    "VSC-only mode allows content with SnapshotHandle",
		},
		{
			name: "VSC-only: No VolumeHandle and no SnapshotHandle - valid (downstream-specific)",
			mode: vscOnlyMode,
			content: &crdv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "vsc-only-content",
					UID:  types.UID("vsc-only-content-uid"),
				},
				Spec: crdv1.VolumeSnapshotContentSpec{
					Source: crdv1.VolumeSnapshotContentSource{},
				},
			},
			expectedResult: true,
			description:    "VSC-only mode allows content without VolumeHandle or SnapshotHandle (downstream-specific, will be skipped in syncContent)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := test.mode.IsValidContentForReconcile(test.content)
			if result != test.expectedResult {
				t.Errorf("Expected %v, got %v: %s", test.expectedResult, result, test.description)
			}
		})
	}
}

func toStringPointer(s string) *string {
	return &s
}
