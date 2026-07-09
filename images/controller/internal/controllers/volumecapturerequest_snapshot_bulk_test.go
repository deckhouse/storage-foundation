/*
Copyright 2026 Flant JSC

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

package controllers

import (
	"testing"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

func TestSnapshotVSCName(t *testing.T) {
	vcrUID := types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	targetUID := "11111111-2222-3333-4444-555555555555"
	got := snapshotVSCName(vcrUID, targetUID)
	want := "snapshot-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-" + targetUIDHash(targetUID)
	if got != want {
		t.Fatalf("snapshotVSCName() = %q, want %q", got, want)
	}
	if len(targetUIDHash(targetUID)) != snapshotTargetHashHexLen {
		t.Fatalf("hash len = %d, want %d", len(targetUIDHash(targetUID)), snapshotTargetHashHexLen)
	}
}

func TestTargetUIDHashDeterministic(t *testing.T) {
	uid := "11111111-2222-3333-4444-555555555555"
	if targetUIDHash(uid) != targetUIDHash(uid) {
		t.Fatal("hash must be deterministic")
	}
	other := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if targetUIDHash(uid) == targetUIDHash(other) {
		t.Fatal("expected different hashes for different target UIDs")
	}
}

func TestMergeVolumeDataBindingsPreservesExisting(t *testing.T) {
	existingA := volumeSnapshotBinding(storagev1alpha1.VolumeCaptureTarget{UID: "uid-a"}, "vsc-a")
	existingB := volumeSnapshotBinding(storagev1alpha1.VolumeCaptureTarget{UID: "uid-b"}, "vsc-b")
	merged := mergeVolumeDataBindings([]storagev1alpha1.VolumeDataBinding{existingA}, []storagev1alpha1.VolumeDataBinding{existingB})
	if len(merged) != 2 {
		t.Fatalf("len = %d, want 2", len(merged))
	}
	updatedA := volumeSnapshotBinding(storagev1alpha1.VolumeCaptureTarget{UID: "uid-a"}, "vsc-a-new")
	merged = mergeVolumeDataBindings(merged, []storagev1alpha1.VolumeDataBinding{updatedA})
	if len(merged) != 2 {
		t.Fatalf("len after update = %d, want 2", len(merged))
	}
	if merged[0].Artifact.Name != "vsc-a-new" || merged[1].Artifact.Name != "vsc-b" {
		t.Fatalf("merge lost binding: %#v", merged)
	}
}

func TestValidateSnapshotTargets(t *testing.T) {
	if err := validateSnapshotTargets(nil); err == nil {
		t.Fatal("expected error for empty targets")
	}
	targets := []storagev1alpha1.VolumeCaptureTarget{
		{UID: "a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "x", Namespace: "ns"},
		{UID: "a", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "y", Namespace: "ns"},
	}
	if err := validateSnapshotTargets(targets); err == nil {
		t.Fatal("expected duplicate uid error")
	}
}

func TestUpsertVolumeDataBinding(t *testing.T) {
	b1 := volumeSnapshotBinding(storagev1alpha1.VolumeCaptureTarget{UID: "a"}, "vsc-a")
	b2 := volumeSnapshotBinding(storagev1alpha1.VolumeCaptureTarget{UID: "b"}, "vsc-b")
	out := upsertVolumeDataBinding([]storagev1alpha1.VolumeDataBinding{b1}, b2)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	b1.Artifact.Name = "vsc-a-updated"
	out = upsertVolumeDataBinding(out, b1)
	if len(out) != 2 || out[0].Artifact.Name != "vsc-a-updated" {
		t.Fatalf("upsert failed: %#v", out)
	}
}
