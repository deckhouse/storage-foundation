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

package dataexport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

var deFixedNow = metav1.NewTime(time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC))

func newDEStatusReconciler(now metav1.Time) *DataexportReconciler {
	return &DataexportReconciler{Now: func() metav1.Time { return now }}
}

func deWith(serverState common.ServerState, ready *metav1.Condition) *dev1alpha1.DataExport {
	de := &dev1alpha1.DataExport{ObjectMeta: metav1.ObjectMeta{Name: "de", Namespace: "ns", Generation: 1}}
	de.Status.ServerState = string(serverState)
	if ready != nil {
		meta.SetStatusCondition(&de.Status.Conditions, *ready)
	}
	return de
}

func readyCond(s metav1.ConditionStatus, r common.ConditionReason) *metav1.Condition {
	return &metav1.Condition{Type: string(common.ConditionReady), Status: s, Reason: string(r)}
}

// TestComputeDataExportPhase covers the DataExport phase machine: Pending → Ready → Expired | Failed
// (+ Terminating). A DataExport has NO Completed phase — a successful export lives at Ready until idle-TTL.
func TestComputeDataExportPhase(t *testing.T) {
	tests := []struct {
		name      string
		de        *dev1alpha1.DataExport
		reconcile error
		want      common.Phase
		mutate    func(*dev1alpha1.DataExport)
	}{
		{
			name: "fresh → Pending",
			de:   deWith("", readyCond(metav1.ConditionFalse, common.ReasonPending)),
			want: common.PhasePending,
		},
		{
			name: "serving (Ready=True) → Ready",
			de:   deWith(common.ServerStateReady, readyCond(metav1.ConditionTrue, common.ReasonServerReady)),
			want: common.PhaseReady,
		},
		{
			name: "serverState IdleExpired → Expired",
			de:   deWith(common.ServerStateIdleExpired, readyCond(metav1.ConditionTrue, common.ReasonServerReady)),
			want: common.PhaseExpired,
		},
		{
			name: "legacy expiry (Ready=False/Expired) → Expired",
			de:   deWith("", readyCond(metav1.ConditionFalse, common.ReasonExpired)),
			want: common.PhaseExpired,
		},
		{
			name:      "terminal reconcile error → Failed",
			de:        deWith(common.ServerStateReady, readyCond(metav1.ConditionFalse, common.ReasonValidationFailed)),
			reconcile: fmt.Errorf("%w: bad spec", ErrTerminal),
			want:      common.PhaseFailed,
		},
		{
			name:      "non-terminal error while serving → stays Ready",
			de:        deWith(common.ServerStateReady, readyCond(metav1.ConditionTrue, common.ReasonServerReady)),
			reconcile: fmt.Errorf("transient: %w", ErrTargetNotReady),
			want:      common.PhaseReady,
		},
		{
			name: "deletion timestamp → Terminating",
			de:   deWith(common.ServerStateReady, readyCond(metav1.ConditionTrue, common.ReasonServerReady)),
			mutate: func(de *dev1alpha1.DataExport) {
				ts := metav1.NewTime(deFixedNow.Add(-time.Minute))
				de.DeletionTimestamp = &ts
				de.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
			},
			want: common.PhaseTerminating,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.mutate != nil {
				tt.mutate(tt.de)
			}
			assert.Equal(t, tt.want, computeDataExportPhase(tt.de, tt.reconcile))
		})
	}
}

func TestComputeDataExportPhase_StickyTerminal(t *testing.T) {
	de := deWith(common.ServerStateReady, readyCond(metav1.ConditionTrue, common.ReasonServerReady))
	de.Status.Phase = string(common.PhaseExpired) // already terminal
	assert.Equal(t, common.PhaseExpired, computeDataExportPhase(de, nil), "settled terminal phase is sticky")
}

func TestFinalizeDataExportStatus_ExpiredStampsOnce(t *testing.T) {
	r := newDEStatusReconciler(deFixedNow)
	de := deWith(common.ServerStateIdleExpired, readyCond(metav1.ConditionFalse, common.ReasonExpired))

	r.finalizeDataExportStatus(de, nil)

	assert.Equal(t, string(common.PhaseExpired), de.Status.Phase)
	require.NotNil(t, de.Status.CompletionTimestamp)
	assert.Equal(t, deFixedNow.Time, de.Status.CompletionTimestamp.Time)

	// Stamped once: a later finalize does not move the timestamp.
	later := newDEStatusReconciler(metav1.NewTime(deFixedNow.Add(time.Hour)))
	later.finalizeDataExportStatus(de, nil)
	assert.Equal(t, deFixedNow.Time, de.Status.CompletionTimestamp.Time)
}

func TestFinalizeDataExportStatus_NonTerminalNoTimestamp(t *testing.T) {
	r := newDEStatusReconciler(deFixedNow)
	de := deWith(common.ServerStateReady, readyCond(metav1.ConditionTrue, common.ReasonServerReady))

	r.finalizeDataExportStatus(de, nil)

	assert.Equal(t, string(common.PhaseReady), de.Status.Phase)
	assert.Nil(t, de.Status.CompletionTimestamp, "no completionTimestamp before a terminal phase")
}

// TestReconcile_DE_StripsLegacyExpiredCondition is the DataExport migration guard: a pre-upgrade
// DataExport carries a stale condition of the removed type "Expired" plus a legacy Ready=True/PodReady
// reason. The controller must strip the out-of-catalog condition and normalize the reason before the first
// status write (otherwise every Status().Update fails enum validation and the object never gets a phase /
// completionTimestamp and is never garbage-collected).
func TestReconcile_DE_StripsLegacyExpiredCondition(t *testing.T) {
	scheme := setupTestScheme()
	de := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:       "1h",
		Publish:   false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindPVC, Name: "test-pvc"},
	})
	// Idle-expired via serverState → the controller drives it to the terminal Expired phase.
	de.Status.ServerState = string(common.ServerStateIdleExpired)
	de.Status.Conditions = []metav1.Condition{
		{Type: "Expired", Status: metav1.ConditionTrue, Reason: "TTLExpired", Message: "legacy", LastTransitionTime: metav1.Now()},
		{Type: string(common.ConditionReady), Status: metav1.ConditionTrue, Reason: "PodReady", Message: "legacy pod", LastTransitionTime: metav1.Now()},
	}
	de.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}

	fakeClient := newFakeClientWithStatus(t, scheme, de)
	r := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-de", Namespace: "test-ns"}})
	require.NoError(t, err)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-de", Namespace: "test-ns"}, got))

	assert.Nil(t, common.GetCondition(got.Status.Conditions, common.ConditionType("Expired")),
		"legacy condition type=Expired must be stripped")
	for _, c := range got.Status.Conditions {
		assert.NotContains(t, []string{"PodReady", "IngressReady"}, c.Reason, "legacy Ready reason must not survive (condition %s)", c.Type)
	}
	assert.Equal(t, string(common.PhaseExpired), got.Status.Phase)
	require.NotNil(t, got.Status.CompletionTimestamp)
}

// TestUpdateDataExport_PreservesConcurrentPodFields mirrors the DataImport no-clobber guarantee for
// DataExport: the exporter pod writes serverState/accessTimestamp/url on its own heartbeat, and
// updateDataExport must re-apply only the controller-owned status onto the freshest object so a concurrent
// serverState=IdleExpired (the DE's critical idle signal) is not clobbered back to empty.
func TestUpdateDataExport_PreservesConcurrentPodFields(t *testing.T) {
	scheme := setupTestScheme()
	access := metav1.NewTime(time.Unix(7000, 0))
	stored := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:       "1h",
		TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindPVC, Name: "test-pvc"},
	})
	stored.Status.ServerState = string(common.ServerStateIdleExpired)
	stored.Status.AccessTimestamp = access
	stored.Status.Url = "https://10.0.0.7:8085/"

	fakeClient := newFakeClientWithStatus(t, scheme, stored)
	r := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	// Reconcile-start snapshot predates the pod write (serverState empty); controller only flips Ready.
	old := createDataExport(dev1alpha1.DataExportSpec{TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindPVC, Name: "test-pvc"}})
	meta.SetStatusCondition(&old.Status.Conditions, metav1.Condition{Type: string(common.ConditionReady), Status: metav1.ConditionFalse, Reason: string(common.ReasonPending)})
	newDE := old.DeepCopy()
	meta.SetStatusCondition(&newDE.Status.Conditions, metav1.Condition{Type: string(common.ConditionReady), Status: metav1.ConditionTrue, Reason: string(common.ReasonServerReady)})

	require.NoError(t, r.updateDataExport(context.Background(), old, newDE))

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-de", Namespace: "test-ns"}, got))
	assert.Equal(t, string(common.ServerStateIdleExpired), got.Status.ServerState, "concurrent serverState=IdleExpired must survive")
	assert.Equal(t, access, got.Status.AccessTimestamp)
	assert.Equal(t, "https://10.0.0.7:8085/", got.Status.Url)
	ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
}

// TestRecoverUserPVC_AlreadyRestoredIsIdempotent is the drift-repair idempotency guard (finding: a healthy
// already-restored PV must not be flagged inconsistent on a re-run). If the controller crashed after
// restoreOriginalPVState stripped the export marker/annotations but before the finalizer removal persisted,
// the next reconcile re-enters recoverUserPVC. Without the guard it would find no original-reclaimPolicy
// annotation, flag the healthy PV inconsistent and wedge cleanup forever (CR stuck Terminating). The guard
// must instead treat the missing export marker as "already restored" and just finish the PVC cleanup.
func TestRecoverUserPVC_AlreadyRestoredIsIdempotent(t *testing.T) {
	scheme := setupTestScheme()
	require.NoError(t, corev1.SchemeBuilder.AddToScheme(scheme))

	userPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "user-pvc",
			Namespace:   "test-ns",
			Annotations: map[string]string{DataExportInProgressKey: "true"},
			Finalizers:  []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: "test-pv"},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	// PV already restored: it carries NO export marker label (and no inconsistent label).
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pv"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			ClaimRef:                      &corev1.ObjectReference{Name: "user-pvc", Namespace: "test-ns"},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv, userPVC).Build()
	r := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	err := r.recoverUserPVC(context.Background(), "test-ns", "user-pvc", dataExportName, pv)
	require.NoError(t, err, "recovering an already-restored PV must be a no-op success, not an error")

	// The healthy PV must NOT be flagged inconsistent.
	gotPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, gotPV))
	assert.NotContains(t, gotPV.Labels, dev1alpha1.LabelPVDataExporterInconsistent,
		"an already-restored PV must never be marked inconsistent")

	// The user PVC cleanup completed idempotently: export annotation + finalizer removed.
	gotPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "test-ns", Name: "user-pvc"}, gotPVC))
	assert.NotContains(t, gotPVC.Annotations, DataExportInProgressKey)
	assert.NotContains(t, gotPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
}

// TestReconcilePodReadyResources_DetectsDeploymentDrift is the drift-repair guard: if the export server
// Deployment is deleted mid-life, the controller must NOT degrade the object to a terminal phase — it flags
// Ready=False/Pending ("re-provisioning") and returns nil so the next reconcile rebuilds the server (the
// idle-TTL clock is not burned by infrastructure downtime).
func TestReconcilePodReadyResources_DetectsDeploymentDrift(t *testing.T) {
	scheme := setupTestScheme()
	de := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:       "1h",
		TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindPVC, Name: "test-pvc"},
	})
	de.Generation = 1
	names := common.NewNames(dev1alpha1.KindPVC, "test-pvc", "test-ns", "test-de")

	// Fake client with NO server Deployment (drift: it was deleted mid-life).
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := createTestReconciler(c, c, createTestConfig())

	err := r.reconcilePodReadyResources(context.Background(), de, names)
	require.NoError(t, err, "a missing server Deployment is repaired, not surfaced as a terminal error")

	ready := meta.FindStatusCondition(de.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, string(common.ReasonPending), ready.Reason)
	assert.Contains(t, ready.Message, "re-provisioning")

	// The phase must stay non-terminal (downtime does not burn the TTL / fail the export).
	assert.False(t, computeDataExportPhase(de, nil).IsTerminal(), "drift must not degrade the phase to a terminal outcome")
}
