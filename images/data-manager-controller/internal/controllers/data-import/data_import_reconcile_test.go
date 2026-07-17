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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

func reconcileScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, storagev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	return scheme
}

func diReq(di *dev1alpha1.DataImport) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: di.Namespace, Name: di.Name}}
}

// createPVCImport builds a CreatePVC DataImport keyed on the given name; the imported bytes land directly
// in the (pre-existing) target PVC. Publish=false keeps the reconcile off the ingress path.
func createPVCImport(name string, serverState common.ServerState) *dev1alpha1.DataImport {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: dev1alpha1.DataImportSpec{
			Mode:        dev1alpha1.DataImportModeCreatePVC,
			Publish:     false,
			Ttl:         "30m",
			PvcTemplate: &dev1alpha1.PersistentVolumeClaimTemplateSpec{PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: name}},
		},
	}
	di.Status.ServerState = string(serverState)
	return di
}

func boundImportPVC(name string) *corev1.PersistentVolumeClaim {
	fs := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &fs},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// TestReconcile_CreatePVC_TranslatesFinishedAndCompletes exercises the real Reconcile end to end for a
// CreatePVC import whose pod reported serverState=Finished. It is the direct guard that the controller
// translates serverState=Finished into UploadFinished=True (with the deduplicated message) — the signal the
// volume populator gates on — and then drives the object to the terminal Completed phase with a
// completionTimestamp. Pre-seeding UploadFinished (as the pure phase-machine tests do) would NOT catch a
// regression in this translation; running the whole reconcile does.
func TestReconcile_CreatePVC_TranslatesFinishedAndCompletes(t *testing.T) {
	scheme := reconcileScheme(t)
	di := createPVCImport("imp-done", common.ServerStateFinished)
	pvc := boundImportPVC("imp-done")

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(di).WithObjects(di, pvc).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "imp-done"}, got))

	// serverState=Finished was translated into the controller-owned UploadFinished condition (message pinned
	// so the dedup decision does not silently regress).
	uf := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionUploadFinished))
	require.NotNil(t, uf, "UploadFinished must be produced from serverState=Finished")
	assert.Equal(t, metav1.ConditionTrue, uf.Status)
	assert.Equal(t, "Client upload finished; producing artifact", uf.Message)

	// The import reached the terminal Completed phase with a stamped completionTimestamp.
	assert.Equal(t, string(common.PhaseCompleted), got.Status.Phase)
	require.NotNil(t, got.Status.CompletionTimestamp)
	completed := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionCompleted))
	require.NotNil(t, completed)
	assert.Equal(t, metav1.ConditionTrue, completed.Status)
}

// TestReconcile_DI_StripsLegacyExpiredCondition is the migration guard (finding: legacy objects hang on
// enum validation). A pre-upgrade DataImport carries a stale condition of the removed type "Expired" and a
// legacy Ready=True/PodReady reason; the narrowed CRD enum no longer permits either. The controller must
// strip the out-of-catalog condition (and normalize the legacy reason) BEFORE the first status write, so
// the object gets a valid new-catalog status, a terminal phase and a completionTimestamp — otherwise the
// GC never reaps it. Removing StripConditionsNotIn from Reconcile makes this test fail.
func TestReconcile_DI_StripsLegacyExpiredCondition(t *testing.T) {
	scheme := reconcileScheme(t)
	di := createPVCImport("legacy-imp", common.ServerStateIdleExpired) // idle-expired → terminal Expired
	meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
		Type: "Expired", Status: metav1.ConditionTrue, Reason: "TTLExpired", Message: "legacy",
	})
	meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
		Type: string(common.ConditionReady), Status: metav1.ConditionTrue, Reason: "PodReady", Message: "legacy pod",
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(di).WithObjects(di).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "legacy-imp"}, got))

	// The stale out-of-catalog condition type is gone.
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, "Expired"),
		"legacy condition type=Expired must be stripped before the first status write")
	// No legacy reason survives anywhere in the conditions list.
	for _, cond := range got.Status.Conditions {
		assert.NotContains(t, []string{"PodReady", "IngressReady"}, cond.Reason,
			"legacy Ready reason must not survive migration (condition %s)", cond.Type)
	}
	// The object reached the terminal Expired phase with a completionTimestamp so the GC can reap it.
	assert.Equal(t, string(common.PhaseExpired), got.Status.Phase)
	require.NotNil(t, got.Status.CompletionTimestamp)
}

// TestReconcile_DI_RetriesOnStatusConflict verifies updateDataImport's RetryOnConflict: a single conflict
// on the status write is retried (re-GET + re-apply) and the reconcile still succeeds and persists. Drop
// the retry wrapper and the first conflict escapes as an error.
func TestReconcile_DI_RetriesOnStatusConflict(t *testing.T) {
	scheme := reconcileScheme(t)
	di := createPVCImport("retry-imp", common.ServerStateFinished)
	pvc := boundImportPVC("retry-imp")

	calls := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(di).WithObjects(di, pvc).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, _ string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				calls++
				if calls == 1 {
					return apierrors.NewConflict(schema.GroupResource{Group: "storage-foundation.deckhouse.io", Resource: "dataimports"}, obj.GetName(), fmt.Errorf("conflict"))
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
		}).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err, "a single status-write conflict must be retried transparently")
	assert.GreaterOrEqual(t, calls, 2, "the status write must have been retried after the injected conflict")

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "retry-imp"}, got))
	assert.Equal(t, string(common.PhaseCompleted), got.Status.Phase)
}

// TestReconcile_DI_SurvivingConflictRequeues covers the noise-plan contract: a conflict that survives the
// update retry (a concurrent writer keeps winning) is benign — Reconcile requeues soon with a nil error
// instead of escalating to an error backoff.
func TestReconcile_DI_SurvivingConflictRequeues(t *testing.T) {
	scheme := reconcileScheme(t)
	di := createPVCImport("conflict-imp", common.ServerStateFinished)
	pvc := boundImportPVC("conflict-imp")

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(di).WithObjects(di, pvc).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: "storage-foundation.deckhouse.io", Resource: "dataimports"}, obj.GetName(), fmt.Errorf("always conflict"))
			},
		}).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

	res, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err, "a surviving conflict must not escalate to an error backoff")
	assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter, "a benign surviving conflict requeues soon")
}

// TestReconcile_DI_TerminalErrorZeroResult covers the noise-plan Result contract: a terminal (ErrTerminal)
// reconcile error records phase=Failed and returns a ZERO Result (no error-requeue) — the object is not
// retried since its spec is immutable.
func TestReconcile_DI_TerminalErrorZeroResult(t *testing.T) {
	scheme := reconcileScheme(t)
	// PopulateData with no storageParams: scratchVolumeParamsFromSpec fails terminally (ErrTerminal),
	// before any dynamic-client (VCR) call.
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-imp", Namespace: "ns"},
		Spec:       dev1alpha1.DataImportSpec{Mode: dev1alpha1.DataImportModePopulateData, Ttl: "30m"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(di).WithObjects(di).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

	res, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err, "a terminal error is recorded as phase=Failed, not surfaced as an error-requeue")
	assert.Equal(t, ctrl.Result{}, res, "terminal reconcile returns a zero Result")

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "bad-imp"}, got))
	assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)
	require.NotNil(t, got.Status.CompletionTimestamp)
}
