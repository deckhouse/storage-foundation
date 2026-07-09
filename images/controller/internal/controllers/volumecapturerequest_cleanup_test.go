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

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func vcrDataRefBinding(targetUID, kind, name, apiVersion string) storagev1alpha1.VolumeDataBinding {
	return storagev1alpha1.VolumeDataBinding{
		TargetUID: targetUID,
		Target: storagev1alpha1.VolumeCaptureTarget{
			UID:        targetUID,
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Namespace:  "default",
			Name:       "pvc-1",
		},
		Artifact: storagev1alpha1.VolumeDataArtifactRef{
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
			DataRefs: []storagev1alpha1.VolumeDataBinding{
				vcrDataRefBinding("uid-1", "VolumeSnapshotContent", "vsc-1", "snapshot.storage.k8s.io/v1"),
			},
		},
	}
	vsc := &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "vsc-1"},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr, vsc).Build()
	controller := &VolumeCaptureRequestController{Client: client}

	if err := controller.cleanupArtifactsForVCR(context.Background(), vcr); err != nil {
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
			DataRefs: []storagev1alpha1.VolumeDataBinding{
				vcrDataRefBinding("uid-2", "VolumeSnapshotContent", "vsc-2", "snapshot.storage.k8s.io/v1"),
			},
		},
	}
	vsc := &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "vsc-2", OwnerReferences: ownerRefs},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr, vsc).Build()
	controller := &VolumeCaptureRequestController{Client: client}

	if err := controller.cleanupArtifactsForVCR(context.Background(), vcr); err != nil {
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
			DataRefs: []storagev1alpha1.VolumeDataBinding{
				vcrDataRefBinding("uid-3", "PersistentVolume", "pv-1", "v1"),
			},
		},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr, pv).Build()
	controller := &VolumeCaptureRequestController{Client: client}

	if err := controller.cleanupArtifactsForVCR(context.Background(), vcr); err != nil {
		t.Fatalf("cleanupArtifactsForVCR failed: %v", err)
	}

	if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "pv-1"}, &corev1.PersistentVolume{}); err == nil {
		t.Fatal("expected PersistentVolume to be deleted")
	}
}

func TestCleanupArtifactsForVCR_TwoDataRefsOrphansDeleted(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	vcr := &storagev1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "vcr-bulk-cleanup", Namespace: "default"},
		Status: storagev1alpha1.VolumeCaptureRequestStatus{
			DataRefs: []storagev1alpha1.VolumeDataBinding{
				vcrDataRefBinding("uid-1", "VolumeSnapshotContent", "vsc-orphan-1", "snapshot.storage.k8s.io/v1"),
				vcrDataRefBinding("uid-2", "VolumeSnapshotContent", "vsc-orphan-2", "snapshot.storage.k8s.io/v1"),
			},
		},
	}
	vsc1 := &snapshotv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "vsc-orphan-1"}}
	vsc2 := &snapshotv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "vsc-orphan-2"}}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr, vsc1, vsc2).Build()
	controller := &VolumeCaptureRequestController{Client: client}

	if err := controller.cleanupArtifactsForVCR(context.Background(), vcr); err != nil {
		t.Fatalf("cleanupArtifactsForVCR failed: %v", err)
	}
	for _, name := range []string{"vsc-orphan-1", "vsc-orphan-2"} {
		if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: name}, &snapshotv1.VolumeSnapshotContent{}); err == nil {
			t.Fatalf("expected VolumeSnapshotContent %s to be deleted", name)
		}
	}
}

func TestCleanupArtifactsForVCR_TwoDataRefsSkipsManaged(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = storagev1alpha1.AddToScheme(scheme)
	_ = snapshotv1.AddToScheme(scheme)

	managedOwner := []metav1.OwnerReference{{
		APIVersion: "test.deckhouse.io/v1alpha1",
		Kind:       "TestSnapshotContent",
		Name:       "content-managed",
		UID:        "uid-managed",
	}}
	orphanOwner := []metav1.OwnerReference{}

	vcr := &storagev1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "vcr-bulk-cleanup-2", Namespace: "default"},
		Status: storagev1alpha1.VolumeCaptureRequestStatus{
			DataRefs: []storagev1alpha1.VolumeDataBinding{
				vcrDataRefBinding("uid-m", "VolumeSnapshotContent", "vsc-managed", "snapshot.storage.k8s.io/v1"),
				vcrDataRefBinding("uid-o", "VolumeSnapshotContent", "vsc-orphan", "snapshot.storage.k8s.io/v1"),
			},
		},
	}
	vscManaged := &snapshotv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "vsc-managed", OwnerReferences: managedOwner}}
	vscOrphan := &snapshotv1.VolumeSnapshotContent{ObjectMeta: metav1.ObjectMeta{Name: "vsc-orphan", OwnerReferences: orphanOwner}}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr, vscManaged, vscOrphan).Build()
	controller := &VolumeCaptureRequestController{Client: client}

	if err := controller.cleanupArtifactsForVCR(context.Background(), vcr); err != nil {
		t.Fatalf("cleanupArtifactsForVCR failed: %v", err)
	}
	if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "vsc-managed"}, &snapshotv1.VolumeSnapshotContent{}); err != nil {
		t.Fatalf("expected managed VSC to remain: %v", err)
	}
	if err := client.Get(context.Background(), ctrlclient.ObjectKey{Name: "vsc-orphan"}, &snapshotv1.VolumeSnapshotContent{}); err == nil {
		t.Fatal("expected orphan VSC to be deleted")
	}
}
