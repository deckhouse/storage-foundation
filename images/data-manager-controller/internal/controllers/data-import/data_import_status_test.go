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

package dataimport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

var fixedNow = metav1.NewTime(time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC))

func newStatusReconciler(now metav1.Time) *DataImportReconciler {
	return &DataImportReconciler{Now: func() metav1.Time { return now }}
}

// diWith builds a DataImport carrying the given Ready/UploadFinished/Completed conditions and serverState.
func diWith(serverState common.ServerState, conds ...metav1.Condition) *dev1alpha1.DataImport {
	di := &dev1alpha1.DataImport{ObjectMeta: metav1.ObjectMeta{Name: "di", Namespace: "ns", Generation: 1}}
	di.Status.ServerState = string(serverState)
	for _, c := range conds {
		meta.SetStatusCondition(&di.Status.Conditions, c)
	}
	return di
}

func cond(t common.ConditionType, s metav1.ConditionStatus, r common.ConditionReason) metav1.Condition {
	return metav1.Condition{Type: string(t), Status: s, Reason: string(r)}
}

func TestComputeDataImportPhase(t *testing.T) {
	r := newStatusReconciler(fixedNow)

	tests := []struct {
		name      string
		di        *dev1alpha1.DataImport
		reconcile error
		wantPhase common.Phase
		mutate    func(*dev1alpha1.DataImport)
	}{
		{
			name:      "fresh object → Pending",
			di:        diWith("", cond(common.ConditionReady, metav1.ConditionFalse, common.ReasonPending)),
			wantPhase: common.PhasePending,
		},
		{
			name:      "server ready (Ready=True) → Ready",
			di:        diWith(common.ServerStateReady, cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady)),
			wantPhase: common.PhaseReady,
		},
		{
			name: "upload finished, capturing (UploadFinished=True, Ready=False) → still Ready",
			di: diWith(common.ServerStateFinished,
				cond(common.ConditionReady, metav1.ConditionFalse, common.ReasonPending),
				cond(common.ConditionUploadFinished, metav1.ConditionTrue, common.ReasonUploadFinished)),
			wantPhase: common.PhaseReady,
		},
		{
			name: "artifact produced (Completed=True) → Completed",
			di: diWith(common.ServerStateFinished,
				cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady),
				cond(common.ConditionCompleted, metav1.ConditionTrue, common.ReasonCompleted)),
			wantPhase: common.PhaseCompleted,
		},
		{
			name:      "serverState IdleExpired → Expired",
			di:        diWith(common.ServerStateIdleExpired, cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady)),
			wantPhase: common.PhaseExpired,
		},
		{
			name:      "terminal reconcile error (ErrTerminal) → Failed",
			di:        diWith(common.ServerStateReady, cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady)),
			reconcile: fmt.Errorf("%w: boom", ErrTerminal),
			wantPhase: common.PhaseFailed,
		},
		{
			name:      "non-terminal reconcile error → stays Ready (no auto-fail)",
			di:        diWith(common.ServerStateReady, cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady)),
			reconcile: fmt.Errorf("transient: %w", ErrTargetFailed),
			wantPhase: common.PhaseReady,
		},
		{
			name: "deletion timestamp → Terminating (overrides everything)",
			di:   diWith(common.ServerStateReady, cond(common.ConditionCompleted, metav1.ConditionTrue, common.ReasonCompleted)),
			mutate: func(di *dev1alpha1.DataImport) {
				ts := metav1.NewTime(fixedNow.Add(-time.Minute))
				di.DeletionTimestamp = &ts
				di.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
			},
			wantPhase: common.PhaseTerminating,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.mutate != nil {
				tt.mutate(tt.di)
			}
			assert.Equal(t, tt.wantPhase, r.computeDataImportPhase(tt.di, tt.reconcile))
		})
	}
}

// TestComputeDataImportPhase_StickyTerminal verifies a settled terminal phase never reverts even when a
// later reconcile sees a transient/ambiguous state — keeping completionTimestamp meaningful and the GC
// clock stable.
func TestComputeDataImportPhase_StickyTerminal(t *testing.T) {
	r := newStatusReconciler(fixedNow)

	di := diWith(common.ServerStateReady, cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady))
	di.Status.Phase = string(common.PhaseCompleted) // already terminal

	// Even with Ready=True and no Completed condition, the persisted terminal phase wins.
	assert.Equal(t, common.PhaseCompleted, r.computeDataImportPhase(di, nil))
}

func TestFinalizeDataImportStatus_CompletedStampsOnce(t *testing.T) {
	r := newStatusReconciler(fixedNow)
	di := diWith(common.ServerStateFinished,
		cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady),
		cond(common.ConditionCompleted, metav1.ConditionTrue, common.ReasonCompleted))

	r.finalizeDataImportStatus(di, nil)

	assert.Equal(t, string(common.PhaseCompleted), di.Status.Phase)
	require.NotNil(t, di.Status.CompletionTimestamp, "completionTimestamp must be set on terminal")
	assert.Equal(t, fixedNow.Time, di.Status.CompletionTimestamp.Time)

	// Ready moved to the terminal Completed reason.
	ready := meta.FindStatusCondition(di.Status.Conditions, string(common.ConditionReady))
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, string(common.ReasonCompleted), ready.Reason)

	// A second finalize with a LATER clock must NOT move completionTimestamp (stamped once).
	later := newStatusReconciler(metav1.NewTime(fixedNow.Add(time.Hour)))
	later.finalizeDataImportStatus(di, nil)
	assert.Equal(t, fixedNow.Time, di.Status.CompletionTimestamp.Time, "completionTimestamp must be stamped once")
}

func TestFinalizeDataImportStatus_Expired(t *testing.T) {
	r := newStatusReconciler(fixedNow)
	di := diWith(common.ServerStateIdleExpired,
		cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady))

	r.finalizeDataImportStatus(di, nil)

	assert.Equal(t, string(common.PhaseExpired), di.Status.Phase)
	assert.Equal(t, fixedNow.Time, di.Status.CompletionTimestamp.Time)
	completed := meta.FindStatusCondition(di.Status.Conditions, string(common.ConditionCompleted))
	assert.Equal(t, metav1.ConditionFalse, completed.Status)
	assert.Equal(t, string(common.ReasonExpired), completed.Reason)
}

func TestFinalizeDataImportStatus_Failed(t *testing.T) {
	r := newStatusReconciler(fixedNow)
	di := diWith(common.ServerStateReady,
		cond(common.ConditionReady, metav1.ConditionFalse, common.ReasonTargetFailed))

	r.finalizeDataImportStatus(di, fmt.Errorf("%w: %w: bad", ErrTerminal, ErrTargetFailed))

	assert.Equal(t, string(common.PhaseFailed), di.Status.Phase)
	assert.Equal(t, fixedNow.Time, di.Status.CompletionTimestamp.Time)
	completed := meta.FindStatusCondition(di.Status.Conditions, string(common.ConditionCompleted))
	assert.Equal(t, metav1.ConditionFalse, completed.Status)
	assert.Equal(t, string(common.ReasonFailed), completed.Reason)
}

// TestFinalizeDataImportStatus_IdempotentTerminal verifies a re-reconcile of a settled terminal object
// produces no condition churn: lastTransitionTime is preserved (no spurious bump).
func TestFinalizeDataImportStatus_IdempotentTerminal(t *testing.T) {
	r := newStatusReconciler(fixedNow)
	di := diWith(common.ServerStateFinished,
		cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady),
		cond(common.ConditionCompleted, metav1.ConditionTrue, common.ReasonCompleted))

	r.finalizeDataImportStatus(di, nil)
	readyLTT := meta.FindStatusCondition(di.Status.Conditions, string(common.ConditionReady)).LastTransitionTime
	completedLTT := meta.FindStatusCondition(di.Status.Conditions, string(common.ConditionCompleted)).LastTransitionTime

	// Re-run finalize with a different clock; a settled terminal must not bump lastTransitionTime.
	r2 := newStatusReconciler(metav1.NewTime(fixedNow.Add(time.Hour)))
	r2.finalizeDataImportStatus(di, nil)

	assert.Equal(t, readyLTT, meta.FindStatusCondition(di.Status.Conditions, string(common.ConditionReady)).LastTransitionTime)
	assert.Equal(t, completedLTT, meta.FindStatusCondition(di.Status.Conditions, string(common.ConditionCompleted)).LastTransitionTime)
}

// TestUpdateDataImport_PreservesConcurrentPodFields is the noise-plan concurrency guarantee: the importer
// pod writes the pod-owned status fields (serverState, accessTimestamp, url, ca) on its own heartbeat.
// updateDataImport re-reads the freshest object and re-applies only the controller-owned status onto it,
// so a serverState=Finished written concurrently (between the reconcile snapshot and the status write)
// is NOT clobbered back to empty — which would drop the Finished signal and hang the import.
func TestUpdateDataImport_PreservesConcurrentPodFields(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dev1alpha1.AddToScheme(scheme))

	access := metav1.NewTime(time.Unix(9000, 0))
	// The object as it currently lives in the API: the pod has just written serverState=Finished +
	// accessTimestamp + url (these landed AFTER the controller took its reconcile snapshot).
	stored := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"},
		Status: dev1alpha1.DataExportImportStatus{
			ServerState:     string(common.ServerStateFinished),
			AccessTimestamp: access,
			Url:             "https://10.0.0.5:8085/",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(stored).WithObjects(stored).Build()

	// The controller's reconcile-start snapshot predates the pod write (serverState empty), and the
	// controller's only change is flipping Ready to True/ServerReady.
	old := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"},
		Status:     dev1alpha1.DataExportImportStatus{},
	}
	meta.SetStatusCondition(&old.Status.Conditions, cond(common.ConditionReady, metav1.ConditionFalse, common.ReasonPending))
	newDI := old.DeepCopy()
	meta.SetStatusCondition(&newDI.Status.Conditions, cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady))

	require.NoError(t, updateDataImport(context.Background(), c, old, newDI))

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "imp"}, got))

	// Pod-owned fields preserved (not clobbered by the controller's snapshot).
	assert.Equal(t, string(common.ServerStateFinished), got.Status.ServerState, "concurrent serverState=Finished must survive")
	assert.Equal(t, access, got.Status.AccessTimestamp, "concurrent accessTimestamp must survive")
	assert.Equal(t, "https://10.0.0.5:8085/", got.Status.Url)
	// Controller-owned change persisted.
	ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, string(common.ReasonServerReady), ready.Reason)
}
