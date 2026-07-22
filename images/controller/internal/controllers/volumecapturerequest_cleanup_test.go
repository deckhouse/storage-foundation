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
	"context"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

func vcrDataBinding(kind, name, apiVersion string) *storagev1alpha1.VolumeDataBinding {
	return &storagev1alpha1.VolumeDataBinding{
		ArtifactRef: storagev1alpha1.VolumeDataArtifactRef{
			APIVersion: apiVersion,
			Kind:       kind,
			Name:       name,
		},
	}
}

func TestCleanupArtifactsForVCR_DeletesOrphans(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	vcr := &storagev1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "vcr-1", Namespace: "default"},
		Status: storagev1alpha1.VolumeCaptureRequestStatus{
			Data: vcrDataBinding("VolumeSnapshotContent", "vsc-1", "snapshot.storage.k8s.io/v1"),
		},
	}
	vsc := &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "vsc-1"},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr, vsc).Build()
	if err := cleanupVCRArtifacts(context.Background(), client, vcr); err != nil {
		t.Fatalf("cleanupArtifactsForVCR failed: %v", err)
	}

	if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "vsc-1"}, &snapshotv1.VolumeSnapshotContent{}); err == nil {
		t.Fatal("expected VolumeSnapshotContent to be deleted")
	}
}

func TestCleanupArtifactsForVCR_SkipsManaged(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	ownerRefs := []metav1.OwnerReference{{
		APIVersion: "test.deckhouse.io/v1alpha1",
		Kind:       "TestSnapshotContent",
		Name:       "content-1",
		UID:        "uid-1",
	}}

	vcr := &storagev1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "vcr-2", Namespace: "default"},
		Status: storagev1alpha1.VolumeCaptureRequestStatus{
			Data: vcrDataBinding("VolumeSnapshotContent", "vsc-2", "snapshot.storage.k8s.io/v1"),
		},
	}
	vsc := &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "vsc-2", OwnerReferences: ownerRefs},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr, vsc).Build()
	if err := cleanupVCRArtifacts(context.Background(), client, vcr); err != nil {
		t.Fatalf("cleanupArtifactsForVCR failed: %v", err)
	}

	if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "vsc-2"}, &snapshotv1.VolumeSnapshotContent{}); err != nil {
		t.Fatalf("expected VolumeSnapshotContent to remain: %v", err)
	}
}

func TestCleanupArtifactsForVCR_DeletesPVOrphans(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	vcr := &storagev1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "vcr-3", Namespace: "default"},
		Status: storagev1alpha1.VolumeCaptureRequestStatus{
			Data: vcrDataBinding("PersistentVolume", "pv-1", "v1"),
		},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr, pv).Build()
	if err := cleanupVCRArtifacts(context.Background(), client, vcr); err != nil {
		t.Fatalf("cleanupArtifactsForVCR failed: %v", err)
	}

	if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "pv-1"}, &corev1.PersistentVolume{}); err == nil {
		t.Fatal("expected PersistentVolume to be deleted")
	}
}

func TestCleanupArtifactsForVCR_NilDataRefNoop(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	vcr := &storagev1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "vcr-nil-dataref", Namespace: "default"},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr).Build()
	if err := cleanupVCRArtifacts(context.Background(), client, vcr); err != nil {
		t.Fatalf("cleanupArtifactsForVCR with nil dataRef must be a no-op, got: %v", err)
	}
}
