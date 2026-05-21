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
	want := "snapshot-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-55555555"
	if got != want {
		t.Fatalf("snapshotVSCName() = %q, want %q", got, want)
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
