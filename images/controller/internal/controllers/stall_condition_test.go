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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

func unobservable(msg string) stallDiagnosis {
	return stallDiagnosis{
		Stalled: true,
		Reason:  storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable,
		Message: msg,
	}
}

func notCompleting(msg string) stallDiagnosis {
	return stallDiagnosis{
		Stalled: true,
		Reason:  storagev1alpha1.ConditionReasonSnapshotExecutionNotCompleting,
		Message: msg,
	}
}

func stalledCondition(conditions []metav1.Condition) *metav1.Condition {
	return meta.FindStatusCondition(conditions, storagev1alpha1.ConditionTypeStalled)
}

func TestApplyStallDiagnosis_ReportsAndClears(t *testing.T) {
	t.Run("reports a stall on its own condition", func(t *testing.T) {
		conditions := []metav1.Condition{readyC(metav1.ConditionFalse, storagev1alpha1.ConditionReasonTargetsPending)}

		applyStallDiagnosis(&conditions, unobservable("nothing picked it up"), metav1.NewTime(vcrNow))

		got := stalledCondition(conditions)
		require.NotNil(t, got)
		assert.Equal(t, metav1.ConditionTrue, got.Status)
		assert.Equal(t, storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable, got.Reason)
		assert.Equal(t, "nothing picked it up", got.Message)
	})

	t.Run("keeps LastTransitionTime while the stall persists", func(t *testing.T) {
		conditions := []metav1.Condition{}
		applyStallDiagnosis(&conditions, unobservable("first"), metav1.NewTime(vcrNow))
		first := stalledCondition(conditions).LastTransitionTime

		applyStallDiagnosis(&conditions, unobservable("second, age grew"), metav1.NewTime(vcrNow.Add(time.Hour)))

		got := stalledCondition(conditions)
		assert.Equal(t, first, got.LastTransitionTime, "LastTransitionTime must answer 'stalled since when'")
		assert.Equal(t, "second, age grew", got.Message, "the message still tracks the current observation")
	})

	t.Run("switches reason in place", func(t *testing.T) {
		conditions := []metav1.Condition{}
		applyStallDiagnosis(&conditions, unobservable("nobody picked it up"), metav1.NewTime(vcrNow))

		applyStallDiagnosis(&conditions, notCompleting("started but never finished"), metav1.NewTime(vcrNow.Add(time.Hour)))

		got := stalledCondition(conditions)
		assert.Equal(t, storagev1alpha1.ConditionReasonSnapshotExecutionNotCompleting, got.Reason)
		assert.Len(t, conditions, 1, "a reason switch must not duplicate the condition")
	})

	t.Run("clears a reported stall as resumed", func(t *testing.T) {
		conditions := []metav1.Condition{}
		applyStallDiagnosis(&conditions, unobservable("nothing picked it up"), metav1.NewTime(vcrNow))

		applyStallDiagnosis(&conditions, stallDiagnosis{}, metav1.NewTime(vcrNow.Add(time.Hour)))

		got := stalledCondition(conditions)
		require.NotNil(t, got)
		assert.Equal(t, metav1.ConditionFalse, got.Status)
		assert.Equal(t, storagev1alpha1.ConditionReasonSnapshotExecutionResumed, got.Reason)
	})

	t.Run("stays silent on a request that never stalled", func(t *testing.T) {
		conditions := []metav1.Condition{readyC(metav1.ConditionFalse, storagev1alpha1.ConditionReasonTargetsPending)}

		applyStallDiagnosis(&conditions, stallDiagnosis{}, metav1.NewTime(vcrNow))

		assert.Nil(t, stalledCondition(conditions), "presence of the condition is itself the signal")
		assert.Len(t, conditions, 1)
	})

	t.Run("never touches Ready", func(t *testing.T) {
		ready := readyC(metav1.ConditionFalse, storagev1alpha1.ConditionReasonTargetsPending)
		ready.Message = "target capture in progress"
		conditions := []metav1.Condition{ready}

		applyStallDiagnosis(&conditions, notCompleting("no result for two hours"), metav1.NewTime(vcrNow))

		got := meta.FindStatusCondition(conditions, storagev1alpha1.ConditionTypeReady)
		require.NotNil(t, got)
		assert.Equal(t, metav1.ConditionFalse, got.Status)
		assert.Equal(t, storagev1alpha1.ConditionReasonTargetsPending, got.Reason)
		assert.Equal(t, "target capture in progress", got.Message)
	})
}

// TestStalledIsNotTerminal is the regression guard for the safety invariant: a stalled request is
// still executing. If a stall diagnosis ever leaks into Ready, reconcile stops and the garbage
// collector reaps a live request together with the snapshot it retains.
func TestStalledIsNotTerminal(t *testing.T) {
	conditions := []metav1.Condition{readyC(metav1.ConditionFalse, storagev1alpha1.ConditionReasonTargetsPending)}
	applyStallDiagnosis(&conditions, unobservable("no observable execution"), metav1.NewTime(vcrNow))

	assert.False(t, isVolumeCaptureTerminal(conditions),
		"Stalled=True must not make the request terminal")

	for _, reason := range []string{
		storagev1alpha1.ConditionReasonSnapshotStackUnavailable,
		storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable,
		storagev1alpha1.ConditionReasonSnapshotExecutionNotCompleting,
		storagev1alpha1.ConditionReasonSnapshotExecutionResumed,
	} {
		assert.NotEqual(t, storagev1alpha1.ConditionReasonTargetsPending, reason,
			"a stall reason must be distinct from the pending reason")
		assert.True(t, isVolumeCaptureTerminal([]metav1.Condition{readyC(metav1.ConditionFalse, reason)}),
			"reason %q on Ready would be read as terminal — which is exactly why it must never be put there", reason)
	}
}

// TestGCKeepsStalledRequests pins the consequence of the invariant end to end: the collector must
// not reap a stalled request even long past its TTL.
func TestGCKeepsStalledRequests(t *testing.T) {
	m := &vcrGCManager{ttl: vcrTTL, now: func() time.Time { return vcrNow }}
	old := vcrNow.Add(-25 * time.Hour)

	stalled := vcr("stalled", readyC(metav1.ConditionFalse, storagev1alpha1.ConditionReasonTargetsPending), &old, false)
	applyStallDiagnosis(&stalled.Status.Conditions, unobservable("no observable execution"), metav1.NewTime(vcrNow))

	assert.False(t, m.ShouldBeDeleted(stalled),
		"a stalled request is still executing: deleting it would drop a snapshot that may yet appear")
}

func TestPatchVCRSnapshotPending_WritesBothConditions(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1alpha1.AddToScheme(scheme))

	pending := &storagev1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "vcr", Namespace: "ns"},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.VolumeCaptureRequest{}).
		WithObjects(pending).
		Build()
	r := &VolumeCaptureRequestController{Client: cl, APIReader: cl, Scheme: scheme}
	ctx := context.Background()
	key := client.ObjectKeyFromObject(pending)

	require.NoError(t, r.patchVCRSnapshotPending(ctx, pending, "", unobservable("no observable execution")))

	got := &storagev1alpha1.VolumeCaptureRequest{}
	require.NoError(t, cl.Get(ctx, key, got))
	ready := meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionTypeReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, storagev1alpha1.ConditionReasonTargetsPending, ready.Reason)
	stalled := stalledCondition(got.Status.Conditions)
	require.NotNil(t, stalled)
	assert.Equal(t, metav1.ConditionTrue, stalled.Status)
	assert.Equal(t, storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable, stalled.Reason)
	assert.False(t, isVolumeCaptureTerminal(got.Status.Conditions))

	// The stall goes away once execution resumes; Ready is unaffected either way.
	require.NoError(t, r.patchVCRSnapshotPending(ctx, pending, "", stallDiagnosis{}))

	require.NoError(t, cl.Get(ctx, key, got))
	stalled = stalledCondition(got.Status.Conditions)
	require.NotNil(t, stalled)
	assert.Equal(t, metav1.ConditionFalse, stalled.Status)
	assert.Equal(t, storagev1alpha1.ConditionReasonSnapshotExecutionResumed, stalled.Reason)
	ready = meta.FindStatusCondition(got.Status.Conditions, storagev1alpha1.ConditionTypeReady)
	assert.Equal(t, storagev1alpha1.ConditionReasonTargetsPending, ready.Reason)
}

func TestFinalizeVCR_ClearsStall(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1alpha1.AddToScheme(scheme))

	completing := &storagev1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "vcr", Namespace: "ns"},
	}
	completing.Status.Conditions = []metav1.Condition{readyC(metav1.ConditionFalse, storagev1alpha1.ConditionReasonTargetsPending)}
	applyStallDiagnosis(&completing.Status.Conditions, notCompleting("slow but alive"), metav1.NewTime(vcrNow))
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.VolumeCaptureRequest{}).
		WithObjects(completing).
		Build()
	r := &VolumeCaptureRequestController{Client: cl, APIReader: cl, Scheme: scheme}
	ctx := context.Background()

	require.NoError(t, r.finalizeVCR(ctx, completing, metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted, "target ready"))

	got := &storagev1alpha1.VolumeCaptureRequest{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(completing), got))
	stalled := stalledCondition(got.Status.Conditions)
	require.NotNil(t, stalled)
	assert.Equal(t, metav1.ConditionFalse, stalled.Status, "a finished request is not stalled")
	assert.True(t, isVolumeCaptureTerminal(got.Status.Conditions))
}

// TestStallThresholdsOrDefault guards against a half-configured override silently mixing a test
// value with a production one.
func TestStallThresholdsOrDefault(t *testing.T) {
	assert.Equal(t, defaultStallThresholds(), (&VolumeCaptureRequestController{}).stallThresholdsOrDefault())

	partial := &VolumeCaptureRequestController{stallGrace: stallThresholds{NoPickup: time.Millisecond}}
	assert.Equal(t, defaultStallThresholds(), partial.stallThresholdsOrDefault())

	full := stallThresholds{NoPickup: time.Millisecond, NoCompletion: time.Second}
	assert.Equal(t, full, (&VolumeCaptureRequestController{stallGrace: full}).stallThresholdsOrDefault())
}
