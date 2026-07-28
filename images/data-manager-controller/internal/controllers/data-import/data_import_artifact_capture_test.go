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
	"errors"
	"testing"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

const (
	captureTestNamespace  = "ns"
	captureTestImportName = "imp-1"
	captureTestImportUID  = types.UID("di-uid")
	captureTestVSCName    = "vsc-1"
)

// artifactCaptureScheme carries every typed API group ensureDataArtifact's real dependencies touch:
// corev1 (scratch PVC), storagev1 (StorageClass, for resolveSnapshotCaptureMode), snapv1
// (VolumeSnapshotClass + the produced VolumeSnapshotContent artifact) and dev1alpha1 (DataImport).
func artifactCaptureScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, storagev1.AddToScheme(scheme))
	require.NoError(t, snapv1.SchemeBuilder.AddToScheme(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	return scheme
}

// readyVolumeCaptureRequest builds the unstructured VolumeCaptureRequest ensureDataArtifact expects once
// capture succeeds: Ready=True/Completed and status.data.artifactRef pointing at the produced
// VolumeSnapshotContent.
func readyVolumeCaptureRequest(name, namespace, artifactName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": volumeCaptureRequestGVR.Group + "/" + volumeCaptureRequestGVR.Version,
		"kind":       volumeCaptureRequestKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{
					"type":   vcrConditionTypeReady,
					"status": string(metav1.ConditionTrue),
					"reason": vcrConditionReasonCompleted,
				},
			},
			"data": map[string]any{
				"artifactRef": map[string]any{
					"apiVersion": "snapshot.storage.k8s.io/v1",
					"kind":       artifactKindVolumeSnapshotContent,
					"name":       artifactName,
					"uid":        "vsc-uid",
				},
			},
		},
	}}
}

// readyObjectKeeper builds the unstructured ObjectKeeper ensureObjectKeeper expects to already exist (with
// a populated UID) so ensureDataArtifact does not requeue waiting on its creation.
func readyObjectKeeper(name string, di *dev1alpha1.DataImport) *unstructured.Unstructured {
	keeper := buildObjectKeeper(name, di)
	keeper.SetUID("keeper-uid")
	return keeper
}

// newArtifactCaptureReconciler builds a DataImportReconciler wired for the PopulateData
// snapshot-capture path: a snapshot-capable StorageClass, a bound+finalized scratch PVC, and a
// Ready/Completed VolumeCaptureRequest + pre-existing ObjectKeeper served over the dynamic client — i.e.
// everything ensureDataArtifact needs to run to completion in one call, without requeuing.
//
// Optional interceptorFuncs are layered onto the typed fake.Client so tests can inject failures (e.g. a
// scratch-PVC Delete error) without duplicating the whole fixture.
func newArtifactCaptureReconciler(t *testing.T, interceptorFuncs ...interceptor.Funcs) (*DataImportReconciler, *corev1.PersistentVolumeClaim) {
	t.Helper()
	scheme := artifactCaptureScheme(t)

	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: captureTestImportName, Namespace: captureTestNamespace, UID: captureTestImportUID},
		Spec: dev1alpha1.DataImportSpec{
			Mode:          dev1alpha1.DataImportModePopulateData,
			StorageParams: &dev1alpha1.StorageParamsSpec{StorageClassName: "fast", Size: "1Gi"},
		},
	}

	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "fast", Annotations: map[string]string{storageClassVSCAnnotation: "fast-vsc"}},
		Provisioner: "csi.example.com",
	}
	vsc := &snapv1.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{Name: "fast-vsc"},
		Driver:     "csi.example.com",
	}

	fs := corev1.PersistentVolumeFilesystem
	scratchPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       captureTestImportName,
			Namespace:  captureTestNamespace,
			UID:        types.UID("pvc-uid"),
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeMode: &fs},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	artifact := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: captureTestVSCName},
		Spec: snapv1.VolumeSnapshotContentSpec{
			DeletionPolicy: snapv1.VolumeSnapshotContentDelete,
			Driver:         "csi.example.com",
			Source:         snapv1.VolumeSnapshotContentSource{VolumeHandle: new(string)},
		},
	}

	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sc, vsc, scratchPVC, artifact)
	for _, f := range interceptorFuncs {
		builder = builder.WithInterceptorFuncs(f)
	}
	c := builder.Build()

	gvrToListKind := map[schema.GroupVersionResource]string{
		objectKeeperGVR:         "ObjectKeeperList",
		volumeCaptureRequestGVR: "VolumeCaptureRequestList",
	}
	keeperName := objectKeeperName(di.UID)
	vcrName := volumeCaptureRequestName(di.UID)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind,
		readyObjectKeeper(keeperName, di),
		readyVolumeCaptureRequest(vcrName, di.Namespace, captureTestVSCName),
	)

	r := &DataImportReconciler{
		Client:     c,
		Dynamic:    dyn,
		dataImport: di,
	}

	// Fetch the scratch PVC back through the client so it carries a real resourceVersion, matching how
	// the real reconcile path (handleTargetStatus -> GetScratchPVC) obtains it before calling
	// ensureDataArtifact.
	got := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: captureTestNamespace, Name: captureTestImportName}, got))
	return r, got
}

// TestEnsureDataArtifact_PopulateData_DeletesScratchPVCOnCompletion is the regression guard for the
// scratch-PVC leak (the fix under test): once ensureDataArtifact successfully produces and anchors the
// durable artifact, the scratch PVC that staged the upload must be gone from the cluster, while
// status.data.artifactRef and Completed=True are still recorded correctly.
func TestEnsureDataArtifact_PopulateData_DeletesScratchPVCOnCompletion(t *testing.T) {
	t.Parallel()

	r, pvc := newArtifactCaptureReconciler(t)

	res, err := r.ensureDataArtifact(context.Background(), pvc)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)

	// (a) the scratch PVC is gone.
	gotPVC := &corev1.PersistentVolumeClaim{}
	getErr := r.Client.Get(context.Background(), types.NamespacedName{Namespace: captureTestNamespace, Name: captureTestImportName}, gotPVC)
	require.Error(t, getErr)
	assert.True(t, apierrors.IsNotFound(getErr), "scratch PVC must be deleted once the artifact is captured")

	// (b) status.data.artifactRef and Completed=True are still set correctly.
	require.NotNil(t, r.dataImport.Status.Data)
	require.NotNil(t, r.dataImport.Status.Data.ArtifactRef)
	assert.Equal(t, captureTestVSCName, r.dataImport.Status.Data.ArtifactRef.Name)
	assert.Equal(t, artifactKindVolumeSnapshotContent, r.dataImport.Status.Data.ArtifactRef.Kind)
	assert.True(t, meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)))

	// The artifact itself is untouched by the scratch-PVC cleanup and still carries the Retain pin.
	gotVSC := &snapv1.VolumeSnapshotContent{}
	require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Name: captureTestVSCName}, gotVSC))
	assert.Equal(t, snapv1.VolumeSnapshotContentRetain, gotVSC.Spec.DeletionPolicy)
}

// TestEnsurePVCImportTarget_DoesNotDeleteTargetPVC is the regression guard for the CreatePVC/PopulateData
// mode split: DeleteScratchPVC must never run on the CreatePVC path, where the target PVC is the user's
// product (preserved on completion and on cleanup), not an internal scratch volume.
func TestEnsurePVCImportTarget_DoesNotDeleteTargetPVC(t *testing.T) {
	t.Parallel()

	r := newCreatePVCReconciler(boundPVC(corev1.PersistentVolumeFilesystem))
	meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
		Type:   string(common.ConditionUploadFinished),
		Status: metav1.ConditionTrue,
		Reason: "UploadFinished",
	})

	res, err := r.ensurePVCImportTarget(context.Background())
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
	assert.True(t, meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)))

	// The target PVC (the user's product) must still be present: ensurePVCImportTarget /
	// handlePVCImportStatus never calls DeleteScratchPVC.
	got := &corev1.PersistentVolumeClaim{}
	require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "restored-pvc"}, got))
}

// TestEnsureDataArtifact_PopulateData_ScratchPVCDeleteErrorBlocksCompletionAndRetries covers the
// updated error-handling contract: a failure while deleting the scratch PVC (here, injected on the
// Delete call itself, after the finalizer-strip Patch has already succeeded) MUST prevent
// ensureDataArtifact from recording Completed this pass -- otherwise the sticky Completed guard in
// Reconcile would mean the PVC is never retried and leaks forever. The error must also be a plain,
// non-terminal error (not wrapped in ErrTerminal) so the standard controller-runtime backoff retries
// it on the next reconcile instead of permanently failing an import whose artifact is already durable.
func TestEnsureDataArtifact_PopulateData_ScratchPVCDeleteErrorBlocksCompletionAndRetries(t *testing.T) {
	t.Parallel()

	injectedErr := errors.New("injected scratch PVC delete failure")
	r, pvc := newArtifactCaptureReconciler(t, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if claim, ok := obj.(*corev1.PersistentVolumeClaim); ok && claim.Name == captureTestImportName {
				return injectedErr
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})

	res, err := r.ensureDataArtifact(context.Background(), pvc)
	require.Error(t, err, "a scratch-PVC delete failure must surface as a reconcile error")
	assert.ErrorIs(t, err, injectedErr)
	assert.False(t, errors.Is(err, ErrTerminal), "a scratch-PVC delete failure must be retryable, not a terminal Failed")
	assert.Zero(t, res, "the error itself drives the retry backoff, not an explicit Result")

	// status.data and Completed=True must NOT be recorded on this pass -- the scratch PVC is not
	// actually gone yet, so declaring the import done here would be exactly the leak this fix closes.
	assert.Nil(t, r.dataImport.Status.Data)
	assert.False(t, meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)))

	// The PVC is still present (the injected error really did prevent deletion, proving this isn't a
	// no-op interceptor), but its finalizer was already stripped before the failed Delete was attempted,
	// so it is immediately deletable by hand even before the next reconcile retries.
	gotPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Namespace: captureTestNamespace, Name: captureTestImportName}, gotPVC))
	assert.NotContains(t, gotPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName,
		"the finalizer must already be stripped even though the subsequent Delete failed")
}
