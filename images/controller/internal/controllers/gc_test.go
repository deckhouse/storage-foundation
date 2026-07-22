/*
Copyright 2025 Flant JSC

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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

var vcrNow = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

const vcrTTL = 24 * time.Hour

func readyC(status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: storagev1alpha1.ConditionTypeReady, Status: status, Reason: reason, LastTransitionTime: metav1.Now()}
}

func vcr(name string, cond metav1.Condition, completion *time.Time, deleting bool) *storagev1alpha1.VolumeCaptureRequest {
	o := &storagev1alpha1.VolumeCaptureRequest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	o.Status.Conditions = []metav1.Condition{cond}
	if completion != nil {
		t := metav1.NewTime(*completion)
		o.Status.CompletionTimestamp = &t
	}
	if deleting {
		t := metav1.NewTime(vcrNow.Add(-time.Minute))
		o.DeletionTimestamp = &t
		o.Finalizers = []string{"test/f"}
	}
	return o
}

func vrr(name string, cond metav1.Condition, completion *time.Time) *storagev1alpha1.VolumeRestoreRequest {
	o := &storagev1alpha1.VolumeRestoreRequest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	o.Status.Conditions = []metav1.Condition{cond}
	if completion != nil {
		t := metav1.NewTime(*completion)
		o.Status.CompletionTimestamp = &t
	}
	return o
}

func tptr(t time.Time) *time.Time { return &t }

func TestAgedOut(t *testing.T) {
	assert.False(t, agedOut(nil, vcrTTL, vcrNow), "nil completion → never aged out (fail-safe)")
	old := metav1.NewTime(vcrNow.Add(-25 * time.Hour))
	assert.True(t, agedOut(&old, vcrTTL, vcrNow))
	boundary := metav1.NewTime(vcrNow.Add(-vcrTTL)) // age == ttl, not > ttl
	assert.False(t, agedOut(&boundary, vcrTTL, vcrNow))
}

func TestVCRGCManager_ShouldBeDeleted(t *testing.T) {
	m := &vcrGCManager{ttl: vcrTTL, now: func() time.Time { return vcrNow }}
	old := vcrNow.Add(-25 * time.Hour)

	assert.True(t, m.ShouldBeDeleted(vcr("done", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &old, false)),
		"terminal (Ready=True) older than TTL is reaped")
	assert.True(t, m.ShouldBeDeleted(vcr("failed", readyC(metav1.ConditionFalse, "SomeError"), &old, false)),
		"Ready=False with a non-pending reason is terminal and reaped")
	assert.False(t, m.ShouldBeDeleted(vcr("in-progress", readyC(metav1.ConditionFalse, storagev1alpha1.ConditionReasonTargetsPending), &old, false)),
		"Ready=False/TargetsPending is NOT terminal (bulk capture still running)")
	assert.False(t, m.ShouldBeDeleted(vcr("fresh", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), tptr(vcrNow.Add(-time.Hour)), false)))
	assert.False(t, m.ShouldBeDeleted(vcr("deleting", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &old, true)))
	assert.False(t, m.ShouldBeDeleted(vcr("no-ts", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), nil, false)))
}

func TestVRRGCManager_ShouldBeDeleted(t *testing.T) {
	m := &vrrGCManager{ttl: vcrTTL, now: func() time.Time { return vcrNow }}
	old := vcrNow.Add(-25 * time.Hour)

	assert.True(t, m.ShouldBeDeleted(vrr("done", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &old)))
	assert.True(t, m.ShouldBeDeleted(vrr("failed", readyC(metav1.ConditionFalse, "SomeError"), &old)),
		"for VRR any settled Ready (True or False) is terminal")
	assert.False(t, m.ShouldBeDeleted(vrr("fresh", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), tptr(vcrNow.Add(-time.Hour)))))
}

func TestVCRGCManager_ListForDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1alpha1.AddToScheme(scheme))
	old := vcrNow.Add(-25 * time.Hour)

	objs := []client.Object{
		vcr("reap", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &old, false),
		vcr("pending", readyC(metav1.ConditionFalse, storagev1alpha1.ConditionReasonTargetsPending), &old, false),
		vcr("fresh", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), tptr(vcrNow.Add(-time.Hour)), false),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	m := &vcrGCManager{client: c, ttl: vcrTTL, now: func() time.Time { return vcrNow }}

	got, err := m.ListForDelete(context.Background(), vcrNow)
	require.NoError(t, err)
	gotNames := make([]string, 0, len(got))
	for _, o := range got {
		gotNames = append(gotNames, o.GetName())
	}
	assert.ElementsMatch(t, []string{"reap"}, gotNames)
}

func TestVRRGCManager_ListForDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1alpha1.AddToScheme(scheme))
	old := vcrNow.Add(-25 * time.Hour)

	objs := []client.Object{
		vrr("reap-done", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &old),
		vrr("reap-failed", readyC(metav1.ConditionFalse, "SomeError"), &old),
		vrr("fresh", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), tptr(vcrNow.Add(-time.Hour))),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	m := &vrrGCManager{client: c, ttl: vcrTTL, now: func() time.Time { return vcrNow }}

	got, err := m.ListForDelete(context.Background(), vcrNow)
	require.NoError(t, err)
	gotNames := make([]string, 0, len(got))
	for _, o := range got {
		gotNames = append(gotNames, o.GetName())
	}
	assert.ElementsMatch(t, []string{"reap-done", "reap-failed"}, gotNames)
}

// TestVCRGCManager_PreDelete_BestEffort verifies the PreDelete contract: orphan-artifact cleanup is
// best-effort — it never returns an error (a failure is logged, and the VCR is still garbage-collected
// instead of wedging in an error-requeue loop). The fixture forces a REAL cleanup failure (so the swallow
// assertion is not vacuous): the VCR points at a VolumeSnapshotContent artifact, but the client's scheme
// deliberately omits snapshotv1, so cleanupVCRArtifacts' Get hits a NoKindMatchError (not NotFound).
func TestVCRGCManager_PreDelete_BestEffort(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1alpha1.AddToScheme(scheme))
	// snapshotv1 intentionally NOT registered → Get(VolumeSnapshotContent) errors.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	m := &vcrGCManager{client: c, ttl: vcrTTL, now: func() time.Time { return vcrNow }}

	old := vcrNow.Add(-25 * time.Hour)
	orphan := vcr("orphan", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &old, false)
	orphan.Status.Data = &storagev1alpha1.VolumeDataBinding{
		ArtifactRef: storagev1alpha1.VolumeDataArtifactRef{Kind: "VolumeSnapshotContent", Name: "orphan-vsc"},
	}

	// Sanity: the fixture really does make cleanup fail — otherwise the swallow assertion below is vacuous.
	require.Error(t, cleanupVCRArtifacts(context.Background(), c, orphan),
		"fixture must trigger a real cleanup error so the best-effort swallow is actually exercised")

	// PreDelete must swallow that error and still return nil (the VCR gets collected anyway).
	assert.NoError(t, m.PreDelete(context.Background(), orphan), "PreDelete must be best-effort and never block GC")
	// A non-VCR object is a clean no-op.
	assert.NoError(t, m.PreDelete(context.Background(), vrr("wrong-type", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &old)),
		"a non-VCR object is a clean no-op")
}
