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
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

const (
	testVSCName    = "snapshot-vcr-uid-abc123"
	testCSIDriver  = "local.csi.storage.deckhouse.io"
	testNoPickup   = 2 * time.Minute
	testNoComplete = 2 * time.Hour
)

var (
	testNow          = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	testThresholds   = stallThresholds{NoPickup: testNoPickup, NoCompletion: testNoComplete}
	testErrorMessage = "rpc error: code = DeadlineExceeded"
)

type vscOption func(*snapshotv1.VolumeSnapshotContent)

// newVSC builds a VolumeSnapshotContent observed `age` after its creation. By default it carries
// the cluster snapshot-controller finalizer and no result — the normal "waiting to be picked up".
func newVSC(age time.Duration, opts ...vscOption) *snapshotv1.VolumeSnapshotContent {
	vsc := &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:              testVSCName,
			CreationTimestamp: metav1.NewTime(testNow.Add(-age)),
			Finalizers:        []string{snapshotContentFinalizer},
		},
		Spec: snapshotv1.VolumeSnapshotContentSpec{Driver: testCSIDriver},
	}
	for _, opt := range opts {
		opt(vsc)
	}
	return vsc
}

func withoutFinalizer() vscOption {
	return func(vsc *snapshotv1.VolumeSnapshotContent) { vsc.Finalizers = nil }
}

func withBeingCreated() vscOption {
	return func(vsc *snapshotv1.VolumeSnapshotContent) {
		vsc.Annotations = map[string]string{annVolumeSnapshotBeingCreated: "yes"}
	}
}

func withReadyToUse() vscOption {
	return func(vsc *snapshotv1.VolumeSnapshotContent) {
		ready := true
		vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{ReadyToUse: &ready}
	}
}

func withStatusError(msg string) vscOption {
	return func(vsc *snapshotv1.VolumeSnapshotContent) {
		vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
			Error: &snapshotv1.VolumeSnapshotError{Message: &msg},
		}
	}
}

// withEmptyStatus covers a written but result-less status: it must be treated exactly like a nil
// status, otherwise the diagnosis would silently disappear.
func withEmptyStatus() vscOption {
	return func(vsc *snapshotv1.VolumeSnapshotContent) {
		vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{}
	}
}

// TestClassifyVSC_StateTable covers every row of the classification table in
// design/capture-preconditions-and-stall-diagnostics.md §5.
func TestClassifyVSC_StateTable(t *testing.T) {
	tests := []struct {
		name       string
		vsc        *snapshotv1.VolumeSnapshotContent
		wantStall  bool
		wantReason string
	}{
		{
			name:       "no snapshot-controller finalizer past pickup grace: stack is not running",
			vsc:        newVSC(testNoPickup+time.Second, withoutFinalizer()),
			wantStall:  true,
			wantReason: storagev1alpha1.ConditionReasonSnapshotStackUnavailable,
		},
		{
			// The content watch classifies a content the moment it appears, and the cluster
			// snapshot-controller needs a moment to add its finalizer. Reporting a dead snapshot
			// stack in that window is a false alarm on every healthy capture.
			name:      "no snapshot-controller finalizer yet, within pickup grace: normal wait",
			vsc:       newVSC(time.Second, withoutFinalizer()),
			wantStall: false,
		},
		{
			name:      "finalizer, no result, no being-created, within pickup grace: normal wait",
			vsc:       newVSC(testNoPickup - time.Second),
			wantStall: false,
		},
		{
			name:       "finalizer, no result, no being-created, past pickup grace: nobody picked it up",
			vsc:        newVSC(testNoPickup + time.Second),
			wantStall:  true,
			wantReason: storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable,
		},
		{
			name:       "empty status is treated as no result",
			vsc:        newVSC(testNoPickup+time.Second, withEmptyStatus()),
			wantStall:  true,
			wantReason: storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable,
		},
		{
			name:      "being-created within completion grace: executor is working",
			vsc:       newVSC(testNoComplete-time.Minute, withBeingCreated()),
			wantStall: false,
		},
		{
			name:       "being-created past completion grace: sent or retried, no result",
			vsc:        newVSC(testNoComplete+time.Minute, withBeingCreated()),
			wantStall:  true,
			wantReason: storagev1alpha1.ConditionReasonSnapshotExecutionNotCompleting,
		},
		{
			name:      "status.error is owned by the VCR-contract track, not by stall diagnostics",
			vsc:       newVSC(72*time.Hour, withStatusError(testErrorMessage)),
			wantStall: false,
		},
		{
			name:      "readyToUse: success is never stalled",
			vsc:       newVSC(72*time.Hour, withReadyToUse()),
			wantStall: false,
		},
		{
			name:      "nil content yields no diagnosis",
			vsc:       nil,
			wantStall: false,
		},
		{
			name: "a content without a creation timestamp has no age to judge",
			vsc: func() *snapshotv1.VolumeSnapshotContent {
				vsc := newVSC(0, withoutFinalizer())
				vsc.CreationTimestamp = metav1.Time{}
				return vsc
			}(),
			wantStall: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyVSC(tt.vsc, testNow, testThresholds)

			assert.Equal(t, tt.wantStall, got.Stalled)
			assert.Equal(t, tt.wantReason, got.Reason)
			if tt.wantStall {
				assert.NotEmpty(t, got.Message, "a stall diagnosis must always carry an operator-facing message")
			} else {
				assert.Empty(t, got.Message, "a non-stalled state must not carry a diagnosis message")
			}
		})
	}
}

// TestClassifyVSC_ThresholdBoundaries pins the exact comparison semantics: the grace period is
// inclusive, so age == threshold already reports a stall.
func TestClassifyVSC_ThresholdBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		vsc        *snapshotv1.VolumeSnapshotContent
		wantStall  bool
		wantReason string
	}{
		{
			name:      "one nanosecond before the pickup grace",
			vsc:       newVSC(testNoPickup - time.Nanosecond),
			wantStall: false,
		},
		{
			name:       "exactly at the pickup grace",
			vsc:        newVSC(testNoPickup),
			wantStall:  true,
			wantReason: storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable,
		},
		{
			name:      "one nanosecond before the completion grace",
			vsc:       newVSC(testNoComplete-time.Nanosecond, withBeingCreated()),
			wantStall: false,
		},
		{
			name:       "exactly at the completion grace",
			vsc:        newVSC(testNoComplete, withBeingCreated()),
			wantStall:  true,
			wantReason: storagev1alpha1.ConditionReasonSnapshotExecutionNotCompleting,
		},
		{
			name:      "being-created past the pickup grace but within the completion grace stays quiet",
			vsc:       newVSC(testNoPickup+time.Minute, withBeingCreated()),
			wantStall: false,
		},
		{
			name:      "one nanosecond before the pickup grace without the finalizer",
			vsc:       newVSC(testNoPickup-time.Nanosecond, withoutFinalizer()),
			wantStall: false,
		},
		{
			name:       "exactly at the pickup grace without the finalizer",
			vsc:        newVSC(testNoPickup, withoutFinalizer()),
			wantStall:  true,
			wantReason: storagev1alpha1.ConditionReasonSnapshotStackUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyVSC(tt.vsc, testNow, testThresholds)

			assert.Equal(t, tt.wantStall, got.Stalled)
			assert.Equal(t, tt.wantReason, got.Reason)
		})
	}
}

// TestClassifyVSC_ResultOutranksMissingFinalizer covers combinations the state table does not
// enumerate: once a result is written, the stack demonstrably worked, so a missing finalizer must
// not be reported as SnapshotStackUnavailable.
func TestClassifyVSC_ResultOutranksMissingFinalizer(t *testing.T) {
	ready := classifyVSC(newVSC(72*time.Hour, withoutFinalizer(), withReadyToUse()), testNow, testThresholds)
	assert.False(t, ready.Stalled)

	failed := classifyVSC(newVSC(72*time.Hour, withoutFinalizer(), withStatusError(testErrorMessage)), testNow, testThresholds)
	assert.False(t, failed.Stalled)
}

// TestClassifyVSC_MessageContent pins the parts of the message an operator needs, and the wording
// the NotCompleting message must NOT use: the being-created annotation carries no timestamp, so
// the age of the CSI call itself is unknown and must not be claimed.
func TestClassifyVSC_MessageContent(t *testing.T) {
	t.Run("unobservable names the content, the driver and the age", func(t *testing.T) {
		got := classifyVSC(newVSC(4*time.Minute+12*time.Second), testNow, testThresholds)

		assert.True(t, got.Stalled)
		assert.Contains(t, got.Message, testVSCName)
		assert.Contains(t, got.Message, testCSIDriver)
		assert.Contains(t, got.Message, "4m12s")
		assert.NotContains(t, got.Message, "snapshotter is absent",
			"absence of a snapshotter is not proven by observation")
	})

	t.Run("not completing describes object age, not RPC duration", func(t *testing.T) {
		got := classifyVSC(newVSC(testNoComplete+14*time.Minute, withBeingCreated()), testNow, testThresholds)

		assert.True(t, got.Stalled)
		assert.Contains(t, got.Message, testVSCName)
		assert.Contains(t, got.Message, testCSIDriver)
		assert.Contains(t, got.Message, "has existed for")
		assert.NotContains(t, got.Message, "RPC")
		assert.NotContains(t, got.Message, "has been running")
	})

	t.Run("stack unavailable points at the snapshot-controller", func(t *testing.T) {
		got := classifyVSC(newVSC(testNoPickup+time.Minute, withoutFinalizer()), testNow, testThresholds)

		assert.True(t, got.Stalled)
		assert.Contains(t, got.Message, testVSCName)
		assert.Contains(t, got.Message, "snapshot-controller")
	})
}

// TestClassifyVSC_IsPureFunction guards the closed input list: the same observed object must yield
// the same diagnosis no matter how many times it is classified, and classification must not mutate
// the object it inspects.
func TestClassifyVSC_IsPureFunction(t *testing.T) {
	vsc := newVSC(testNoPickup + time.Minute)
	before := vsc.DeepCopy()

	first := classifyVSC(vsc, testNow, testThresholds)
	second := classifyVSC(vsc, testNow, testThresholds)

	assert.Equal(t, first, second, "repeated classification must not depend on history")
	assert.Equal(t, before, vsc, "classification must not mutate the observed object")
}

// TestDefaultStallThresholds pins the shipped defaults and their relative order: the completion
// grace must be far larger, because snapshotting a large volume is legitimately slow.
func TestDefaultStallThresholds(t *testing.T) {
	got := defaultStallThresholds()

	assert.Equal(t, 2*time.Minute, got.NoPickup)
	assert.Equal(t, 2*time.Hour, got.NoCompletion)
	assert.Greater(t, got.NoCompletion, got.NoPickup)
}
