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

func TestVolumeSnapshotBindingSetsTargetUID(t *testing.T) {
	target := storagev1alpha1.VolumeCaptureTarget{UID: "uid-a", Namespace: "ns", Name: "pvc-a"}
	binding := volumeSnapshotBinding(target, "vsc-a", "vsc-uid-a")
	if binding.TargetUID != "uid-a" {
		t.Fatalf("TargetUID = %q, want %q", binding.TargetUID, "uid-a")
	}
	if binding.Target.Namespace != "ns" {
		t.Fatalf("Target.Namespace = %q, want %q", binding.Target.Namespace, "ns")
	}
	if binding.Artifact.Name != "vsc-a" || binding.Artifact.Kind != "VolumeSnapshotContent" {
		t.Fatalf("unexpected artifact: %#v", binding.Artifact)
	}
	if binding.Artifact.UID != "vsc-uid-a" {
		t.Fatalf("Artifact.UID = %q, want %q", binding.Artifact.UID, "vsc-uid-a")
	}
}
