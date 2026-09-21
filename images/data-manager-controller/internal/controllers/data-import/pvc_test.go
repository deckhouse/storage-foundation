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

package dataimport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

// pvcFinalizerTestScheme carries the API groups this file's fixtures need: corev1 for the PVC and
// dev1alpha1 for the DataImport that owns it.
func pvcFinalizerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	return scheme
}

// pvcFinalizerTestPVC builds a PVC carrying only our finalizer -- the shape RemovePVCFinalizer,
// DeleteScratchPVC and patchPVCRemoveFinalizer all operate on.
func pvcFinalizerTestPVC(namespace, name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  namespace,
			Name:       name,
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
	}
}

// pvcFinalizerTestOwnerDataImport builds a minimal CreatePVC DataImport whose spec.pvcTemplate names
// pvcName, for tests that need a caller to pass into RemovePVCFinalizer / getTargetPVC.
func pvcFinalizerTestOwnerDataImport(namespace, name, pvcName string) *dev1alpha1.DataImport {
	return &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: dev1alpha1.DataImportSpec{
			Mode:        dev1alpha1.DataImportModeCreatePVC,
			TTL:         "30m",
			PvcTemplate: &dev1alpha1.PersistentVolumeClaimTemplateSpec{PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: pvcName}},
		},
	}
}

// pvcFinalizerTestConcurrentWriterFinalizer is a foreign finalizer used to prove a rejected patch never
// silently drops a concurrent writer's own entry from metadata.finalizers.
const pvcFinalizerTestConcurrentWriterFinalizer = "other.example.com/concurrent-writer"

// pvcFinalizerTestStaleBaseInterceptor simulates a concurrent writer racing the caller: the first Patch
// call issued against the named PVC is preceded by an out-of-band Update that adds a foreign finalizer
// (bumping resourceVersion), so the caller's own Patch -- built from a base snapshot taken before that
// write -- fails the optimistic-lock precondition exactly as a genuine concurrent writer would trigger.
func pvcFinalizerTestStaleBaseInterceptor(namespace, name string) interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			pvc, ok := obj.(*corev1.PersistentVolumeClaim)
			if !ok || pvc.Namespace != namespace || pvc.Name != name {
				return cl.Patch(ctx, obj, patch, opts...)
			}

			live := &corev1.PersistentVolumeClaim{}
			if err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, live); err != nil {
				return err
			}
			if controllerutil.AddFinalizer(live, pvcFinalizerTestConcurrentWriterFinalizer) {
				if err := cl.Update(ctx, live); err != nil {
					return err
				}
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	}
}

// TestRemovePVCFinalizerUsesOptimisticLock guards the patch-discipline fix this branch applies to every
// finalizer-removal path on the CreatePVC target PVC / PopulateData scratch PVC: a stale base must surface
// apierrors.IsConflict to the caller, never be retried inside the helper, and never silently drop a
// concurrent writer's own finalizer entry (the write that a plain client.MergeFrom -- with no optimistic
// lock -- would have accepted and clobbered).
func TestRemovePVCFinalizerUsesOptimisticLock(t *testing.T) {
	t.Parallel()

	const testNamespace = "ns"
	const testPVCName = "target-pvc"

	assertFinalizerNotDropped := func(t *testing.T, c client.Client) {
		t.Helper()
		got := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, got))
		assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName,
			"our own finalizer must still be present -- the rejected patch must not have applied")
		assert.Contains(t, got.Finalizers, pvcFinalizerTestConcurrentWriterFinalizer,
			"the concurrent writer's finalizer must survive -- a merge patch built from a stale base must never clobber it")
	}

	t.Run("error: RemovePVCFinalizer surfaces a Conflict from a stale base", func(t *testing.T) {
		t.Parallel()

		di := pvcFinalizerTestOwnerDataImport(testNamespace, "owner", testPVCName)
		pvc := pvcFinalizerTestPVC(testNamespace, testPVCName)
		pvc.Annotations = map[string]string{
			dev1alpha1.AnnotationStorageManagerNamespaceKey: di.Namespace,
			dev1alpha1.AnnotationStorageManagerNameKey:      di.Name,
		}
		c := fake.NewClientBuilder().WithScheme(pvcFinalizerTestScheme(t)).WithObjects(pvc).
			WithInterceptorFuncs(pvcFinalizerTestStaleBaseInterceptor(testNamespace, testPVCName)).Build()

		err := RemovePVCFinalizer(context.Background(), c, di, dev1alpha1.StorageManagerFinalizerName)
		require.Error(t, err, "a stale base must surface as an error, not be silently absorbed")
		assert.True(t, apierrors.IsConflict(err), "the surfaced error must be a Conflict, not retried inside RemovePVCFinalizer")

		assertFinalizerNotDropped(t, c)
	})

	t.Run("error: DeleteScratchPVC's finalizer patch surfaces a Conflict from a stale base", func(t *testing.T) {
		t.Parallel()

		pvc := pvcFinalizerTestPVC(testNamespace, testPVCName)
		c := fake.NewClientBuilder().WithScheme(pvcFinalizerTestScheme(t)).WithObjects(pvc).
			WithInterceptorFuncs(pvcFinalizerTestStaleBaseInterceptor(testNamespace, testPVCName)).Build()

		// DeleteScratchPVC takes the base snapshot from the object handed to it, so the caller must pass a
		// freshly fetched copy -- exactly as teardownImportInfra does -- for the interceptor's race to bite.
		fresh := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, fresh))

		err := DeleteScratchPVC(context.Background(), c, fresh)
		require.Error(t, err, "a stale base must surface as an error, not be silently absorbed")
		assert.True(t, apierrors.IsConflict(err), "the surfaced error must be a Conflict, not retried inside DeleteScratchPVC")

		assertFinalizerNotDropped(t, c)

		// The PVC must still exist: Delete is only reached once the finalizer-strip patch succeeds.
		got := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, got))
	})

	t.Run("error: patchPVCRemoveFinalizer surfaces a Conflict from a stale base", func(t *testing.T) {
		t.Parallel()

		pvc := pvcFinalizerTestPVC(testNamespace, testPVCName)
		c := fake.NewClientBuilder().WithScheme(pvcFinalizerTestScheme(t)).WithObjects(pvc).
			WithInterceptorFuncs(pvcFinalizerTestStaleBaseInterceptor(testNamespace, testPVCName)).Build()

		fresh := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, fresh))
		base := fresh.DeepCopy()
		controllerutil.RemoveFinalizer(fresh, dev1alpha1.StorageManagerFinalizerName)

		err := patchPVCRemoveFinalizer(context.Background(), c, fresh, base)
		require.Error(t, err, "a stale base must surface as an error, not be silently absorbed")
		assert.True(t, apierrors.IsConflict(err), "patchPVCRemoveFinalizer, the shared primitive, must not retry the conflict itself")

		assertFinalizerNotDropped(t, c)
	})

	t.Run("error: a Conflict raised inside reconcileTargetPVCRelease surfaces from Reconcile unretried", func(t *testing.T) {
		t.Parallel()

		di := &dev1alpha1.DataImport{
			ObjectMeta: metav1.ObjectMeta{Name: "release-conflict", Namespace: testNamespace},
			Spec: dev1alpha1.DataImportSpec{
				Mode:        dev1alpha1.DataImportModeCreatePVC,
				TTL:         "30m",
				PvcTemplate: &dev1alpha1.PersistentVolumeClaimTemplateSpec{PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: testPVCName}},
			},
		}
		di.Status.Phase = string(common.PhaseFailed)
		di.Status.CompletionTimestamp = &fixedNow
		pvcFinalizerTestSeedFailedConditions(di)

		pvc := pvcFinalizerTestPVC(testNamespace, testPVCName)
		pvc.Annotations = map[string]string{
			dev1alpha1.AnnotationStorageManagerNamespaceKey: testNamespace,
			dev1alpha1.AnnotationStorageManagerNameKey:      di.Name,
		}

		c := fake.NewClientBuilder().WithScheme(pvcFinalizerTestScheme(t)).WithStatusSubresource(di).WithObjects(di, pvc).
			WithInterceptorFuncs(pvcFinalizerTestStaleBaseInterceptor(testNamespace, testPVCName)).Build()

		// The target PVC is "Terminating": deleted by the user while it still carries our finalizer, exactly
		// how the release step's own precondition (DeletionTimestamp != nil) is reached in production.
		require.NoError(t, c.Delete(context.Background(), pvc))

		r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.Error(t, err, "a Conflict raised inside reconcileTargetPVCRelease must surface out of Reconcile, not be swallowed or retried internally")
		assert.True(t, apierrors.IsConflict(err))

		assertFinalizerNotDropped(t, c)
	})
}

// TestRemovePVCFinalizerOwnershipGuard guards the fix that made RemovePVCFinalizer share
// getTargetPVC/isTargetPVCOwnedBy with reconcileTargetPVCRelease: deleting a DataImport whose
// spec.pvcTemplate names a PVC owned by a *different* DataImport (the second import terminally failed on
// the ownership guard in ensureImportPVC, e.g. a name collision) must never strip that PVC's finalizer —
// doing so would let the true owner's target PVC be deleted out from under an in-flight upload, which
// then recreates it on its next pass, silently discarding the user's delete.
func TestRemovePVCFinalizerOwnershipGuard(t *testing.T) {
	t.Parallel()

	const testNamespace = "ns"
	const testPVCName = "target-pvc"

	tests := []struct {
		name          string
		buildPVC      func() *corev1.PersistentVolumeClaim
		caller        *dev1alpha1.DataImport
		wantFinalizer bool
	}{
		{
			name: "success: caller owns the PVC, finalizer is removed",
			buildPVC: func() *corev1.PersistentVolumeClaim {
				pvc := pvcFinalizerTestPVC(testNamespace, testPVCName)
				pvc.Annotations = map[string]string{
					dev1alpha1.AnnotationStorageManagerNamespaceKey: testNamespace,
					dev1alpha1.AnnotationStorageManagerNameKey:      "owner",
				}
				return pvc
			},
			caller:        pvcFinalizerTestOwnerDataImport(testNamespace, "owner", testPVCName),
			wantFinalizer: false,
		},
		{
			name: "success: PVC owned by a different DataImport is left untouched",
			buildPVC: func() *corev1.PersistentVolumeClaim {
				pvc := pvcFinalizerTestPVC(testNamespace, testPVCName)
				pvc.Annotations = map[string]string{
					dev1alpha1.AnnotationStorageManagerNamespaceKey: testNamespace,
					dev1alpha1.AnnotationStorageManagerNameKey:      "owner",
				}
				return pvc
			},
			caller:        pvcFinalizerTestOwnerDataImport(testNamespace, "stranger", testPVCName),
			wantFinalizer: true,
		},
		{
			name: "success: PVC carrying no storage-manager annotations is left untouched",
			buildPVC: func() *corev1.PersistentVolumeClaim {
				return pvcFinalizerTestPVC(testNamespace, testPVCName)
			},
			caller:        pvcFinalizerTestOwnerDataImport(testNamespace, "stranger", testPVCName),
			wantFinalizer: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pvc := tt.buildPVC()
			c := fake.NewClientBuilder().WithScheme(pvcFinalizerTestScheme(t)).WithObjects(pvc).Build()

			require.NoError(t, RemovePVCFinalizer(context.Background(), c, tt.caller, dev1alpha1.StorageManagerFinalizerName))

			got := &corev1.PersistentVolumeClaim{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, got))
			assert.Equal(t, tt.wantFinalizer, controllerutil.ContainsFinalizer(got, dev1alpha1.StorageManagerFinalizerName))
		})
	}
}

// pvcFinalizerTestSeedFailedConditions seeds the minimal Ready=False/TargetFailed condition a persisted
// Failed-phase DataImport already carries, so Reconcile's migration/defaulting steps at the top
// (StripConditionsNotIn, the legacy-reason rewrite) have nothing to change and the object reaches the
// release step in one pass.
func pvcFinalizerTestSeedFailedConditions(di *dev1alpha1.DataImport) {
	di.Status.Conditions = []metav1.Condition{
		{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonTargetFailed),
			Message:            "target PVC is being deleted",
			ObservedGeneration: di.Generation,
			LastTransitionTime: fixedNow,
		},
		{
			Type:               string(common.ConditionCompleted),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonInProgress),
			Message:            "Data import in progress",
			ObservedGeneration: di.Generation,
			LastTransitionTime: fixedNow,
		},
	}
}
