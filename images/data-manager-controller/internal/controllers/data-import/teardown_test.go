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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

// teardownControllerNS is the ControllerNamespace used by every fixture in this file: the importer
// Deployment, its CA secret and (for PopulateData) the internal scratch PVC all live here.
const teardownControllerNS = "d8"

// teardownScheme carries every typed API group teardownImportInfra/cleanupDataImport touch: corev1
// (PVC/Secret/Service/Pod), appsv1 (Deployment), batchv1 (dummy Job), networkingv1 (Ingress) and
// dev1alpha1 (DataImport).
func teardownScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	return scheme
}

// teardownDataImport builds a DataImport fixture for exercising teardownImportInfra/cleanupDataImport
// directly, without driving the whole upload/capture pipeline.
func teardownDataImport(name string, mode dev1alpha1.DataImportMode) *dev1alpha1.DataImport {
	return &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name + "-uid")},
		Spec:       dev1alpha1.DataImportSpec{Mode: mode, Ttl: "30m"},
	}
}

// newTeardownReconciler builds a DataImportReconciler wired the same way Reconcile wires it (r.dataImport
// + r.names set from the DataImport identity), plus a dynamic fake client carrying dynObjs (typically
// VolumeCaptureRequest fixtures), so teardownImportInfra/cleanupDataImport/deleteVolumeCaptureRequest can
// be exercised directly without driving the whole Reconcile.
func newTeardownReconciler(
	t *testing.T,
	di *dev1alpha1.DataImport,
	extra []client.Object,
	dynObjs []runtime.Object,
	interceptorFuncs ...interceptor.Funcs,
) (*DataImportReconciler, client.Client, *dynamicfake.FakeDynamicClient) {
	t.Helper()

	builder := fake.NewClientBuilder().WithScheme(teardownScheme(t)).WithObjects(extra...)
	for _, f := range interceptorFuncs {
		builder = builder.WithInterceptorFuncs(f)
	}
	c := builder.Build()

	gvrToListKind := map[schema.GroupVersionResource]string{volumeCaptureRequestGVR: "VolumeCaptureRequestList"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, dynObjs...)

	r := &DataImportReconciler{
		Client:     c,
		Reader:     c,
		Dynamic:    dyn,
		Config:     &config.Options{ControllerNamespace: teardownControllerNS},
		Now:        func() metav1.Time { return fixedNow },
		dataImport: di,
		names:      common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name),
	}
	return r, c, dyn
}

// TestTeardownImportInfra_DeletesImporterDeploymentAndCASecret guards the shape of the rewritten
// teardownImportInfra: it must actually delete the importer Deployment and the CA secret (a pre-existing
// leak DataImport never closed before) in the controller namespace, for both import modes, and report
// allGone once nothing is left.
func TestTeardownImportInfra_DeletesImporterDeploymentAndCASecret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode dev1alpha1.DataImportMode
	}{
		{name: "PopulateData", mode: dev1alpha1.DataImportModePopulateData},
		{name: "CreatePVC", mode: dev1alpha1.DataImportModeCreatePVC},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			di := teardownDataImport("teardown-imp-"+tt.name, tt.mode)
			names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)

			deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: names.DeployName, Namespace: teardownControllerNS}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: names.CASecretName, Namespace: teardownControllerNS}}

			r, c, _ := newTeardownReconciler(t, di, []client.Object{deploy, secret}, nil)

			// The CA secret's prior existence gates allGone this pass (mirroring the pre-existing dummy-Job
			// contract: common.DeleteJob reports the Job "existed" on the very pass it deletes it) -- so the
			// first call deletes both objects but must still report allGone=false.
			allGone, err := r.teardownImportInfra(context.Background())
			require.NoError(t, err)
			assert.False(t, allGone, "the CA secret existed this pass, so allGone must stay false even though it (and the Deployment) were just deleted")

			getErr := c.Get(context.Background(), types.NamespacedName{Namespace: teardownControllerNS, Name: names.DeployName}, &appsv1.Deployment{})
			require.Error(t, getErr)
			assert.True(t, apierrors.IsNotFound(getErr), "the importer Deployment must be deleted")

			getErr = c.Get(context.Background(), types.NamespacedName{Namespace: teardownControllerNS, Name: names.CASecretName}, &corev1.Secret{})
			require.Error(t, getErr)
			assert.True(t, apierrors.IsNotFound(getErr), "the CA secret must be deleted")

			allGone, err = r.teardownImportInfra(context.Background())
			require.NoError(t, err)
			assert.True(t, allGone, "the next pass, with everything already gone, reports allGone=true")
		})
	}

	// allGone only flips true on the pass where nothing was observed to still exist -- mirroring the
	// pre-existing dummy-Job contract (common.DeleteJob reports the Job "existed" on the very pass it
	// deletes it), which teardownImportInfra's rewrite deliberately preserves.
	t.Run("success: allGone flips true only on the pass after a live resource's prior existence is confirmed gone", func(t *testing.T) {
		t.Parallel()

		di := teardownDataImport("teardown-flip", dev1alpha1.DataImportModePopulateData)
		names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: names.DummyJobName, Namespace: di.Namespace}}

		r, c, _ := newTeardownReconciler(t, di, []client.Object{job}, nil)

		allGone, err := r.teardownImportInfra(context.Background())
		require.NoError(t, err)
		assert.False(t, allGone, "the dummy Job existed this pass, so allGone must stay false even though it was just deleted")

		getErr := c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: names.DummyJobName}, &batchv1.Job{})
		require.Error(t, getErr)
		assert.True(t, apierrors.IsNotFound(getErr), "the dummy Job must actually be deleted on the very pass that reports allGone=false")

		allGone, err = r.teardownImportInfra(context.Background())
		require.NoError(t, err)
		assert.True(t, allGone, "the next pass, with nothing left existing, reports allGone=true")
	})
}

// TestTeardownImportInfra_DoesNotDeletePVCWhileImporterPodRemains guards the mount-consistency invariant
// at the teardown call site: the internal scratch PVC must never be deleted while an importer pod could
// still hold the mount.
func TestTeardownImportInfra_DoesNotDeletePVCWhileImporterPodRemains(t *testing.T) {
	t.Parallel()

	di := teardownDataImport("teardown-pod-race", dev1alpha1.DataImportModePopulateData)
	names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
	scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, teardownControllerNS)
	pod := importerPodFixture("importer-pod", teardownControllerNS, names.DeployName)

	r, c, _ := newTeardownReconciler(t, di, []client.Object{scratchPVC, pod}, nil)

	allGone, err := r.teardownImportInfra(context.Background())
	require.NoError(t, err)
	assert.False(t, allGone, "a live importer pod means the mount may still be held")

	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: teardownControllerNS, Name: names.ImportScratchPVCName}, &corev1.PersistentVolumeClaim{}),
		"the internal scratch PVC must survive while an importer pod remains")

	require.NoError(t, c.Delete(context.Background(), pod))

	allGone, err = r.teardownImportInfra(context.Background())
	require.NoError(t, err)
	assert.True(t, allGone)

	getErr := c.Get(context.Background(),
		types.NamespacedName{Namespace: teardownControllerNS, Name: names.ImportScratchPVCName}, &corev1.PersistentVolumeClaim{})
	require.Error(t, getErr)
	assert.True(t, apierrors.IsNotFound(getErr), "the internal scratch PVC must be deleted once the importer pod is gone")
}

// TestTeardownImportInfra_CreatePVCNeverDeletesTargetPVC is the mode-split guard: teardownImportInfra
// must never touch the CreatePVC target PVC (the user's product) -- only its import finalizer is removed
// later, by cleanupDataImport.
func TestTeardownImportInfra_CreatePVCNeverDeletesTargetPVC(t *testing.T) {
	t.Parallel()

	di := teardownDataImport("teardown-createpvc", dev1alpha1.DataImportModeCreatePVC)
	di.Spec.PvcTemplate = &dev1alpha1.PersistentVolumeClaimTemplateSpec{
		PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: "target-pvc"},
	}
	targetPVC := boundImportPVC("target-pvc")
	targetPVC.Namespace = di.Namespace
	targetPVC.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}

	r, c, _ := newTeardownReconciler(t, di, []client.Object{targetPVC}, nil)

	allGone, err := r.teardownImportInfra(context.Background())
	require.NoError(t, err)
	assert.True(t, allGone)

	got := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: "target-pvc"}, got),
		"teardownImportInfra must never delete the CreatePVC target PVC")
	assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName,
		"the finalizer is only removed later, by cleanupDataImport")
}

// TestCleanupDataImport_DeletesVolumeCaptureRequest guards the explicit VCR delete added to
// cleanupDataImport: the VCR is deleted (and a repeat delete on an already-gone VCR stays a clean no-op)
// only once teardownImportInfra reports allGone -- while any resource still blocks allGone, neither the
// VCR nor the DataImport's own finalizer are touched.
func TestCleanupDataImport_DeletesVolumeCaptureRequest(t *testing.T) {
	t.Parallel()

	t.Run("success: VCR deleted and finalizer removed once allGone", func(t *testing.T) {
		t.Parallel()

		di := teardownDataImport("cleanup-vcr", dev1alpha1.DataImportModePopulateData)
		di.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
		vcrName := volumeCaptureRequestName(di.UID)
		vcr := readyVolumeCaptureRequest(vcrName, teardownControllerNS, "vsc-x")

		r, _, dyn := newTeardownReconciler(t, di, nil, []runtime.Object{vcr})

		done, err := r.cleanupDataImport(context.Background())
		require.NoError(t, err)
		assert.True(t, done, "allGone must be true once every server-side resource is gone")

		_, getErr := dyn.Resource(volumeCaptureRequestGVR).Namespace(teardownControllerNS).Get(context.Background(), vcrName, metav1.GetOptions{})
		require.Error(t, getErr)
		assert.True(t, apierrors.IsNotFound(getErr), "the VolumeCaptureRequest must be deleted once the import is cleaned up")
		assert.NotContains(t, r.dataImport.Finalizers, dev1alpha1.StorageManagerFinalizerName,
			"the DataImport finalizer is removed (in-memory; Reconcile's deferred block persists it) once teardown reports allGone")

		// A second call, with the VCR already gone, must remain a clean no-op.
		done, err = r.cleanupDataImport(context.Background())
		require.NoError(t, err)
		assert.True(t, done)
	})

	t.Run("success: VCR and finalizer untouched while a live importer pod blocks allGone", func(t *testing.T) {
		t.Parallel()

		di := teardownDataImport("cleanup-vcr-blocked", dev1alpha1.DataImportModePopulateData)
		di.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
		names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
		pod := importerPodFixture("importer-pod", teardownControllerNS, names.DeployName)
		vcrName := volumeCaptureRequestName(di.UID)
		vcr := readyVolumeCaptureRequest(vcrName, teardownControllerNS, "vsc-y")

		r, _, dyn := newTeardownReconciler(t, di, []client.Object{pod}, []runtime.Object{vcr})

		done, err := r.cleanupDataImport(context.Background())
		require.NoError(t, err)
		assert.False(t, done, "allGone must be false while a live importer pod blocks teardown")

		_, getErr := dyn.Resource(volumeCaptureRequestGVR).Namespace(teardownControllerNS).Get(context.Background(), vcrName, metav1.GetOptions{})
		require.NoError(t, getErr, "the VCR must survive while a live importer pod blocks allGone")
		assert.Contains(t, r.dataImport.Finalizers, dev1alpha1.StorageManagerFinalizerName,
			"the finalizer must not be removed while allGone is false")
	})
}

// TestCleanupDataImport_ConflictSurfacesForRequeue pins updateDataImport's documented conflict contract
// on the deletion/cleanup path specifically: a single conflict on the finalizer-removal write is retried
// transparently (re-GET + re-apply), and a conflict that survives every retry is a benign requeue, not a
// raw error -- cleanupDataImport itself must not add its own retry loop on top.
func TestCleanupDataImport_ConflictSurfacesForRequeue(t *testing.T) {
	t.Parallel()

	newDeletingReconciler := func(t *testing.T, di *dev1alpha1.DataImport, interceptorFuncs interceptor.Funcs) *DataImportReconciler {
		t.Helper()
		scheme := teardownScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(di).WithObjects(di).
			WithInterceptorFuncs(interceptorFuncs).Build()

		gvrToListKind := map[schema.GroupVersionResource]string{volumeCaptureRequestGVR: "VolumeCaptureRequestList"}
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind)

		return &DataImportReconciler{
			Client: c, Reader: c, Dynamic: dyn,
			Config: &config.Options{ControllerNamespace: teardownControllerNS},
			Now:    func() metav1.Time { return fixedNow },
		}
	}

	t.Run("success: a single conflict on the finalizer-removal write is retried transparently", func(t *testing.T) {
		t.Parallel()

		di := teardownDataImport("cleanup-conflict-retry", dev1alpha1.DataImportModePopulateData)
		di.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
		now := fixedNow
		di.DeletionTimestamp = &now

		calls := 0
		r := newDeletingReconciler(t, di, interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*dev1alpha1.DataImport); ok {
					calls++
					if calls == 1 {
						return apierrors.NewConflict(schema.GroupResource{Group: "storage-foundation.deckhouse.io", Resource: "dataimports"}, obj.GetName(), fmt.Errorf("conflict"))
					}
				}
				return cl.Update(ctx, obj, opts...)
			},
		})

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err, "a single conflict on the finalizer-removal write must be retried transparently")
		assert.GreaterOrEqual(t, calls, 2, "the metadata write must have been retried after the injected conflict")

		getErr := r.Client.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, &dev1alpha1.DataImport{})
		require.Error(t, getErr, "once its last finalizer is removed the object is fully deleted")
		assert.True(t, apierrors.IsNotFound(getErr))
	})

	t.Run("success: a surviving conflict requeues instead of escalating to an error", func(t *testing.T) {
		t.Parallel()

		di := teardownDataImport("cleanup-conflict-surviving", dev1alpha1.DataImportModePopulateData)
		di.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
		now := fixedNow
		di.DeletionTimestamp = &now

		r := newDeletingReconciler(t, di, interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: "storage-foundation.deckhouse.io", Resource: "dataimports"}, obj.GetName(), fmt.Errorf("always conflict"))
			},
		})

		res, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err, "a surviving conflict must not escalate to an error backoff")
		assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter, "a benign surviving conflict requeues soon")
	})
}
