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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
	sfsnapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
)

func newVolumeSnapshotDomainLifecycleReconciler(
	t *testing.T,
	vs *sfsnapv1.VolumeSnapshot,
	coreObjects ...client.Object,
) *VolumeSnapshotDomainReconciler {
	t.Helper()

	coreScheme := apiruntime.NewScheme()
	if err := corev1.AddToScheme(coreScheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	coreClient := fake.NewClientBuilder().WithScheme(coreScheme).WithObjects(coreObjects...).Build()

	snapScheme := apiruntime.NewScheme()
	if err := sfsnapv1.AddToScheme(snapScheme); err != nil {
		t.Fatalf("add sfsnapv1: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(snapScheme); err != nil {
		t.Fatalf("add state-snapshotter v1alpha1: %v", err)
	}
	snapClient := fake.NewClientBuilder().
		WithScheme(snapScheme).
		WithStatusSubresource(&sfsnapv1.VolumeSnapshot{}).
		WithObjects(vs).
		Build()

	return &VolumeSnapshotDomainReconciler{
		Client:     coreClient,
		APIReader:  coreClient,
		SnapClient: snapClient,
	}
}

func managedCaptureVolumeSnapshot(name, pvcName string) *sfsnapv1.VolumeSnapshot {
	return &sfsnapv1.VolumeSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: sfsnapv1.SchemeGroupVersion.String(),
			Kind:       sfsnapv1.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test",
			UID:       types.UID(name + "-uid"),
			Labels: map[string]string{
				labelForkProcessed:   "true",
				labelSnapshotManaged: managedValueTrue,
			},
		},
		Spec: sfsnapv1.VolumeSnapshotSpec{
			Source: sfsnapv1.VolumeSnapshotSource{PersistentVolumeClaimName: &pvcName},
		},
	}
}

func reconcileVolumeSnapshot(t *testing.T, r *VolumeSnapshotDomainReconciler, name string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "test", Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile VolumeSnapshot: %v", err)
	}
	return result
}

func getDomainVolumeSnapshot(t *testing.T, r *VolumeSnapshotDomainReconciler, name string) *sfsnapv1.VolumeSnapshot {
	t.Helper()
	vs := &sfsnapv1.VolumeSnapshot{}
	if err := r.SnapClient.Get(
		context.Background(),
		types.NamespacedName{Namespace: "test", Name: name},
		vs,
	); err != nil {
		t.Fatalf("get VolumeSnapshot: %v", err)
	}
	return vs
}

func TestVolumeSnapshotDomainWaitsInPlanningForMissingPVC(t *testing.T) {
	vs := managedCaptureVolumeSnapshot("snapshot-waiting", "source-pvc")
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs)

	result := reconcileVolumeSnapshot(t, r, vs.Name)
	if result.RequeueAfter != volumeSnapshotDomainArtifactRequeueAfter {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, volumeSnapshotDomainArtifactRequeueAfter)
	}

	got := getDomainVolumeSnapshot(t, r, vs.Name)
	state := volumeSnapshotAdapter{snap: got}.GetDomainCaptureState()
	if state.Phase != snapshotsdk.PhasePlanning {
		t.Fatalf("phase = %q, want %q", state.Phase, snapshotsdk.PhasePlanning)
	}
	if !strings.Contains(state.Message, `PersistentVolumeClaim "source-pvc"`) {
		t.Fatalf("message = %q, want missing PVC diagnostic", state.Message)
	}

	requests := &ssv1alpha1.ManifestCaptureRequestList{}
	if err := r.SnapClient.List(context.Background(), requests, client.InNamespace("test")); err != nil {
		t.Fatalf("list ManifestCaptureRequests: %v", err)
	}
	if len(requests.Items) != 0 {
		t.Fatalf("ManifestCaptureRequests = %d, want 0", len(requests.Items))
	}
}

func TestVolumeSnapshotDomainPublishesFinishedWhenCoreCaptureCompletes(t *testing.T) {
	captured := false
	vs := managedCaptureVolumeSnapshot("snapshot-finished", "source-pvc")
	vs.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
		CommonController: &storagev1alpha1.CommonControllerCaptureState{
			ManifestCaptured: &captured,
			DataCaptured:     &captured,
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pvc", Namespace: "test", UID: "pvc-uid"},
	}
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs, pvc)

	result := reconcileVolumeSnapshot(t, r, vs.Name)
	if result.RequeueAfter != volumeSnapshotDomainRequeueAfter {
		t.Fatalf("initial RequeueAfter = %s, want %s", result.RequeueAfter, volumeSnapshotDomainRequeueAfter)
	}

	got := getDomainVolumeSnapshot(t, r, vs.Name)
	state := volumeSnapshotAdapter{snap: got}.GetDomainCaptureState()
	if state.Phase != snapshotsdk.PhasePlanned {
		t.Fatalf("initial phase = %q, want %q", state.Phase, snapshotsdk.PhasePlanned)
	}
	if state.ManifestCaptureRequestName == "" {
		t.Fatal("manifestCaptureRequestName must be published")
	}
	if got.Status.SourceRef == nil || got.Status.SourceRef.Name != pvc.Name || got.Status.SourceRef.UID != pvc.UID {
		t.Fatalf("sourceRef = %#v, want PVC identity", got.Status.SourceRef)
	}

	captured = true
	got.Status.CaptureState.CommonController.ManifestCaptured = &captured
	got.Status.CaptureState.CommonController.DataCaptured = &captured
	if err := r.SnapClient.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("update core capture latches: %v", err)
	}

	result = reconcileVolumeSnapshot(t, r, vs.Name)
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("completed result = %#v, want no requeue", result)
	}
	got = getDomainVolumeSnapshot(t, r, vs.Name)
	state = volumeSnapshotAdapter{snap: got}.GetDomainCaptureState()
	if state.Phase != snapshotsdk.PhaseFinished {
		t.Fatalf("completed phase = %q, want %q", state.Phase, snapshotsdk.PhaseFinished)
	}
}

func TestVolumeSnapshotDomainStopsOnCoreFailureAfterPlannedWithoutLivePVC(t *testing.T) {
	captured := false
	vs := managedCaptureVolumeSnapshot("snapshot-core-failed", "deleted-pvc")
	vs.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
		CommonController: &storagev1alpha1.CommonControllerCaptureState{
			ManifestCaptured: &captured,
			DataCaptured:     &captured,
		},
		DomainSpecificController: &storagev1alpha1.DomainSpecificControllerCaptureState{
			ManifestCaptureRequestName: "existing-mcr",
			Phase:                      snapshotsdk.PhasePlanned,
			ExcludedRefs:               []storagev1alpha1.ExcludedObjectRef{},
		},
	}
	vs.Status.Conditions = []metav1.Condition{{
		Type:    storagev1alpha1.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  "VolumeCaptureFailed",
		Message: "native CSI capture failed",
	}}
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs)

	result := reconcileVolumeSnapshot(t, r, vs.Name)
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("result = %#v, want no requeue", result)
	}

	got := getDomainVolumeSnapshot(t, r, vs.Name)
	state := volumeSnapshotAdapter{snap: got}.GetDomainCaptureState()
	if state.Phase != snapshotsdk.PhasePlanned {
		t.Fatalf("phase = %q, want core-owned failure to leave %q", state.Phase, snapshotsdk.PhasePlanned)
	}
	if state.Reason != "" || state.Message != "" {
		t.Fatalf("domain reason/message = %q/%q, want core failure left solely in Ready", state.Reason, state.Message)
	}
}
