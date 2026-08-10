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
	"strings"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

func stalledVCR(name string, uid types.UID, targetUID, reason string) *storagev1alpha1.VolumeCaptureRequest {
	vcr := snapshotVCR(name, uid, targetUID)
	vcr.Status.Conditions = []metav1.Condition{readyC(metav1.ConditionFalse, storagev1alpha1.ConditionReasonTargetsPending)}
	applyStallDiagnosis(&vcr.Status.Conditions, stallDiagnosis{Stalled: true, Reason: reason, Message: "diagnosed"}, metav1.NewTime(vcrNow))
	return vcr
}

func contentForVCR(vcr *storagev1alpha1.VolumeCaptureRequest, driver string) *snapshotv1.VolumeSnapshotContent {
	return &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: snapshotVSCName(vcr.UID, vcr.Spec.Target.UID)},
		Spec:       snapshotv1.VolumeSnapshotContentSpec{Driver: driver},
	}
}

func newCollectorClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1alpha1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestShouldEmitStallEvent(t *testing.T) {
	stalled := unobservable("no observable execution")
	other := notCompleting("no result for hours")

	tests := []struct {
		name      string
		previous  *metav1.Condition
		diagnosis stallDiagnosis
		want      bool
	}{
		{
			name:      "entering a stall is reported",
			previous:  nil,
			diagnosis: stalled,
			want:      true,
		},
		{
			name:      "entering a stall after a previous one was cleared is reported again",
			previous:  &metav1.Condition{Status: metav1.ConditionFalse, Reason: storagev1alpha1.ConditionReasonSnapshotExecutionResumed},
			diagnosis: stalled,
			want:      true,
		},
		{
			name:      "the same diagnosis is not repeated",
			previous:  &metav1.Condition{Status: metav1.ConditionTrue, Reason: stalled.Reason},
			diagnosis: stalled,
			want:      false,
		},
		{
			name:      "switching reason moves attention elsewhere and is reported",
			previous:  &metav1.Condition{Status: metav1.ConditionTrue, Reason: stalled.Reason},
			diagnosis: other,
			want:      true,
		},
		{
			name:      "leaving a stall is visible on the condition and needs no event",
			previous:  &metav1.Condition{Status: metav1.ConditionTrue, Reason: stalled.Reason},
			diagnosis: stallDiagnosis{},
			want:      false,
		},
		{
			name:      "a request that was never stalled produces nothing",
			previous:  nil,
			diagnosis: stallDiagnosis{},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldEmitStallEvent(tt.previous, tt.diagnosis))
		})
	}
}

func TestStallCollector(t *testing.T) {
	const header = `
# HELP storage_foundation_volume_capture_stalled Number of VolumeCaptureRequests whose snapshot execution shows no observable progress.
# TYPE storage_foundation_volume_capture_stalled gauge
`

	t.Run("counts stalled requests by reason and driver", func(t *testing.T) {
		first := stalledVCR("first", "uid-1", "pvc-1", storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable)
		second := stalledVCR("second", "uid-2", "pvc-2", storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable)
		third := stalledVCR("third", "uid-3", "pvc-3", storagev1alpha1.ConditionReasonSnapshotStackUnavailable)
		cl := newCollectorClient(t,
			first, second, third,
			contentForVCR(first, "ceph.rbd.csi.ceph.com"),
			contentForVCR(second, "ceph.rbd.csi.ceph.com"),
			contentForVCR(third, "local.csi.storage.deckhouse.io"),
		)

		expected := header + `
storage_foundation_volume_capture_stalled{driver="ceph.rbd.csi.ceph.com",reason="SnapshotExecutionUnobservable"} 2
storage_foundation_volume_capture_stalled{driver="local.csi.storage.deckhouse.io",reason="SnapshotStackUnavailable"} 1
`
		require.NoError(t, testutil.CollectAndCompare(newStallCollector(cl), strings.NewReader(expected), stalledMetricName))
	})

	t.Run("series disappear once the stall is over", func(t *testing.T) {
		resumed := stalledVCR("resumed", "uid-1", "pvc-1", storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable)
		applyStallDiagnosis(&resumed.Status.Conditions, stallDiagnosis{}, metav1.NewTime(vcrNow.Add(time.Hour)))
		cl := newCollectorClient(t, resumed, contentForVCR(resumed, "ceph.rbd.csi.ceph.com"))

		assert.Equal(t, 0, testutil.CollectAndCount(newStallCollector(cl), stalledMetricName),
			"a mutable gauge would have left a permanent false 1 here")
	})

	t.Run("a healthy cluster produces no series at all", func(t *testing.T) {
		healthy := snapshotVCR("healthy", "uid-1", "pvc-1")
		healthy.Status.Conditions = []metav1.Condition{readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted)}
		cl := newCollectorClient(t, healthy)

		assert.Equal(t, 0, testutil.CollectAndCount(newStallCollector(cl), stalledMetricName))
	})

	t.Run("a stalled request whose content is unreadable is still counted", func(t *testing.T) {
		orphan := stalledVCR("orphan", "uid-1", "pvc-1", storagev1alpha1.ConditionReasonSnapshotStackUnavailable)
		cl := newCollectorClient(t, orphan)

		expected := header + `
storage_foundation_volume_capture_stalled{driver="unknown",reason="SnapshotStackUnavailable"} 1
`
		require.NoError(t, testutil.CollectAndCompare(newStallCollector(cl), strings.NewReader(expected), stalledMetricName))
	})

	t.Run("request and content names never become labels", func(t *testing.T) {
		vcr := stalledVCR("a-very-specific-request-name", "uid-1", "pvc-1", storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable)
		cl := newCollectorClient(t, vcr, contentForVCR(vcr, "ceph.rbd.csi.ceph.com"))
		registry := prometheus.NewPedanticRegistry()
		require.NoError(t, registry.Register(newStallCollector(cl)))

		families, err := registry.Gather()
		require.NoError(t, err)
		require.Len(t, families, 1)

		for _, metric := range families[0].GetMetric() {
			names := make([]string, 0, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				names = append(names, label.GetName())
				assert.NotEqual(t, vcr.Name, label.GetValue(), "request names are unbounded and must stay out of labels")
				assert.NotEqual(t, snapshotVSCName(vcr.UID, vcr.Spec.Target.UID), label.GetValue(),
					"content names are unbounded and must stay out of labels")
			}
			assert.ElementsMatch(t, []string{"driver", "reason"}, names)
		}
	})
}

// TestSnapshotPollInterval pins the safety-net pacing: it starts where the old fixed polling was,
// grows monotonically, and stops at a ceiling that is only as high as the watch is proven to cover.
func TestSnapshotPollInterval(t *testing.T) {
	assert.Equal(t, snapshotPollInitialInterval, snapshotPollInterval(0))
	assert.Equal(t, snapshotPollInitialInterval, snapshotPollInterval(snapshotPollBackoffAfter-time.Second))
	assert.Equal(t, 10*time.Second, snapshotPollInterval(snapshotPollBackoffAfter))
	assert.Equal(t, 20*time.Second, snapshotPollInterval(time.Minute))
	assert.Equal(t, 40*time.Second, snapshotPollInterval(2*time.Minute))
	assert.Equal(t, snapshotPollMaxInterval, snapshotPollInterval(time.Hour))
	assert.Equal(t, snapshotPollMaxInterval, snapshotPollInterval(72*time.Hour),
		"the interval is bounded: a stalled request keeps being re-checked forever")

	previous := time.Duration(0)
	for age := time.Duration(0); age < 10*time.Minute; age += 5 * time.Second {
		current := snapshotPollInterval(age)
		assert.GreaterOrEqual(t, current, previous, "pacing must never speed up as the wait grows")
		assert.LessOrEqual(t, current, snapshotPollMaxInterval)
		previous = current
	}
}
