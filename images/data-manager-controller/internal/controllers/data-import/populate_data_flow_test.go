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
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

// flowControllerNamespace is the ControllerNamespace used by every fixture in this file: the internal
// scratch PVC, the importer Deployment, the VolumeCaptureRequest and the pods all live here for
// PopulateData, never in the DataImport's own namespace.
const flowControllerNamespace = "d8"

// populateFlowScheme extends reconcileScheme with the VolumeSnapshotClass type
// resolveSnapshotCaptureMode needs to resolve snapshot capability for the scratch PVC's StorageClass.
func populateFlowScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := reconcileScheme(t)
	require.NoError(t, snapv1.SchemeBuilder.AddToScheme(s))
	return s
}

// wffcVolumeSnapshotClass is the VolumeSnapshotClass referenced by wffcStorageClass's annotation; its
// Driver must match that StorageClass's Provisioner for resolveSnapshotCaptureMode to succeed.
func wffcVolumeSnapshotClass() *snapv1.VolumeSnapshotClass {
	return &snapv1.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{Name: "wffc-vsc"},
		Driver:     "csi.example.com",
	}
}

// newPopulateDataImport builds a PopulateData DataImport staged on the "wffc" StorageClass (snapshot
// capable, matching wffcStorageClass/wffcVolumeSnapshotClass).
func newPopulateDataImport(name string) *dev1alpha1.DataImport {
	return &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name + "-uid")},
		Spec: dev1alpha1.DataImportSpec{
			Mode:    dev1alpha1.DataImportModePopulateData,
			Publish: false,
			Ttl:     "30m",
			StorageParams: &dev1alpha1.StorageParamsSpec{
				StorageClassName: "wffc",
				Size:             "1Gi",
				VolumeMode:       "Filesystem",
			},
		},
	}
}

// withUploadFinished sets UploadFinished=True directly (bypassing the serverState=Finished translation
// Reconcile normally performs), so fixtures can start straight in the capture phase.
func withUploadFinished(di *dev1alpha1.DataImport) *dev1alpha1.DataImport {
	meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
		Type:   string(common.ConditionUploadFinished),
		Status: metav1.ConditionTrue,
		Reason: string(common.ReasonUploadFinished),
	})
	return di
}

// boundInternalScratchPVC builds an already-Bound, finalized internal scratch PVC as it would exist in
// the controller namespace once the upload phase has bound it.
func boundInternalScratchPVC(name, namespace string) *corev1.PersistentVolumeClaim {
	fs := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeMode: &fs},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// pendingOnPVCCreateInterceptor models the real API server's binder controller, which flips a freshly
// created PVC to phase=Pending within moments: the fake client leaves status.phase empty forever, which
// (unlike a real cluster) internalPVCStatus would then classify as Failed rather than Pending.
func pendingOnPVCCreateInterceptor() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := cl.Create(ctx, obj, opts...); err != nil {
				return err
			}
			pvc, ok := obj.(*corev1.PersistentVolumeClaim)
			if !ok || pvc.Status.Phase != "" {
				return nil
			}
			pvc.Status.Phase = corev1.ClaimPending
			return cl.Status().Update(ctx, pvc)
		},
	}
}

// newPopulateDataFlowReconciler builds a DataImportReconciler wired the same way cmd/main.go wires it
// (typed client + dynamic client for VolumeCaptureRequest/ObjectKeeper), so tests can drive the real
// Reconcile end to end.
func newPopulateDataFlowReconciler(
	t *testing.T,
	objs []client.Object,
	dynObjs []runtime.Object,
	interceptorFuncs ...interceptor.Funcs,
) (*DataImportReconciler, client.Client, *dynamicfake.FakeDynamicClient) {
	t.Helper()

	builder := fake.NewClientBuilder().WithScheme(populateFlowScheme(t))
	for _, o := range objs {
		if di, ok := o.(*dev1alpha1.DataImport); ok {
			builder = builder.WithStatusSubresource(di)
		}
	}
	builder = builder.WithObjects(objs...)
	for _, f := range interceptorFuncs {
		builder = builder.WithInterceptorFuncs(f)
	}
	c := builder.Build()

	gvrToListKind := map[schema.GroupVersionResource]string{
		objectKeeperGVR:         "ObjectKeeperList",
		volumeCaptureRequestGVR: "VolumeCaptureRequestList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, dynObjs...)

	r := &DataImportReconciler{
		Client:  c,
		Reader:  c,
		Dynamic: dyn,
		Config:  &config.Options{ControllerNamespace: flowControllerNamespace},
		Now:     func() metav1.Time { return fixedNow },
	}
	return r, c, dyn
}

// TestReconcile_PopulateData_NothingIsCreatedInTheUserNamespace is the headline guard for the redesign:
// on a snapshot-capable WFFC StorageClass, the very first reconcile must create nothing operational in
// the user's namespace -- the internal scratch PVC and its importer Deployment live entirely in the
// controller namespace.
func TestReconcile_PopulateData_NothingIsCreatedInTheUserNamespace(t *testing.T) {
	t.Parallel()

	di := newPopulateDataImport("flow-imp-1")
	objs := []client.Object{di, wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace)}
	r, c, dyn := newPopulateDataFlowReconciler(t, objs, nil, pendingOnPVCCreateInterceptor())

	res, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)
	assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)

	pvcList := &corev1.PersistentVolumeClaimList{}
	require.NoError(t, c.List(context.Background(), pvcList, client.InNamespace(di.Namespace)))
	assert.Empty(t, pvcList.Items, "no PVC must be created in the user's namespace")

	jobList := &batchv1.JobList{}
	require.NoError(t, c.List(context.Background(), jobList, client.InNamespace(di.Namespace)))
	assert.Empty(t, jobList.Items, "no Job must be created in the user's namespace")

	podList := &corev1.PodList{}
	require.NoError(t, c.List(context.Background(), podList, client.InNamespace(di.Namespace)))
	assert.Empty(t, podList.Items, "no Pod must be created in the user's namespace")

	vcrList, err := dyn.Resource(volumeCaptureRequestGVR).Namespace(di.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, vcrList.Items, "no VolumeCaptureRequest must be created in the user's namespace")

	scratchPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: flowControllerNamespace, Name: r.names.ImportScratchPVCName}, scratchPVC))
	assert.Nil(t, scratchPVC.Spec.DataSourceRef, "the internal scratch PVC must have no DataSourceRef -- the importer Deployment, not the populator, is its first consumer")
	assert.Equal(t, dev1alpha1.LabelDataImportValue, scratchPVC.Labels[dev1alpha1.LabelApplicationKey])
	assert.Equal(t, di.Namespace, scratchPVC.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey])
	assert.Equal(t, di.Name, scratchPVC.Annotations[dev1alpha1.AnnotationStorageManagerNameKey])

	deploy := &appsv1.Deployment{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: flowControllerNamespace, Name: r.names.DeployName}, deploy))

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
	ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, string(common.ReasonPVCCreated), ready.Reason)
}

// TestReconcile_PopulateData_NoDummyJobOnWaitForFirstConsumer is the explicit regression guard for the
// dummy-Job removal: even on a WaitForFirstConsumer StorageClass, with the internal PVC staying Pending
// across two reconciles, no batchv1.Job is ever created in either namespace.
func TestReconcile_PopulateData_NoDummyJobOnWaitForFirstConsumer(t *testing.T) {
	t.Parallel()

	di := newPopulateDataImport("flow-imp-2")
	objs := []client.Object{di, wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace)}
	r, c, _ := newPopulateDataFlowReconciler(t, objs, nil, pendingOnPVCCreateInterceptor())

	assertNoJobsAnywhere := func() {
		jobList := &batchv1.JobList{}
		require.NoError(t, c.List(context.Background(), jobList))
		assert.Empty(t, jobList.Items, "PopulateData must never create a dummy consumer Job, even under WaitForFirstConsumer")
	}

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)
	assertNoJobsAnywhere()

	_, err = r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)
	assertNoJobsAnywhere()
}

// TestReconcile_PopulateData_UploadServerCreatedBeforePVCBinds guards the ordering that makes
// WaitForFirstConsumer resolve: the importer Deployment (the PVC's first consumer) must already exist
// while the internal PVC is still Pending.
func TestReconcile_PopulateData_UploadServerCreatedBeforePVCBinds(t *testing.T) {
	t.Parallel()

	di := newPopulateDataImport("flow-imp-3")
	objs := []client.Object{di, wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace)}
	r, c, _ := newPopulateDataFlowReconciler(t, objs, nil, pendingOnPVCCreateInterceptor())

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	scratchPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: flowControllerNamespace, Name: r.names.ImportScratchPVCName}, scratchPVC))
	assert.Equal(t, corev1.ClaimPending, scratchPVC.Status.Phase, "the internal PVC must still be Pending on the very first pass")

	deploy := &appsv1.Deployment{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: flowControllerNamespace, Name: r.names.DeployName}, deploy),
		"the importer Deployment must already exist while the PVC is still Pending -- that ordering is what resolves WaitForFirstConsumer")
}

// TestReconcile_PopulateData_StopsImporterBeforeCapture is the single most important new test: the
// importer must be fully stopped (Deployment gone AND no pod left) before any VolumeCaptureRequest is
// created, on every code path. Without this the volume could be captured while a live writer still holds
// the mount, silently corrupting the snapshot (images/data-exporter has no explicit fsync anywhere).
func TestReconcile_PopulateData_StopsImporterBeforeCapture(t *testing.T) {
	t.Parallel()

	di := newPopulateDataImport("flow-imp-4")
	withUploadFinished(di)
	names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
	scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, flowControllerNamespace)

	objs := []client.Object{di, wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace), scratchPVC}
	keeper := readyObjectKeeper(objectKeeperName(di.UID), di)
	r, c, dyn := newPopulateDataFlowReconciler(t, objs, []runtime.Object{keeper})

	// Seed a live importer Deployment (via the real production helper, so its shape is faithful) and pod,
	// as if a prior reconcile had already brought the upload server up.
	r.dataImport = di
	r.names = names
	require.NoError(t, r.ensureImporterDeployment(context.Background(), scratchPVC))
	pod := importerPodFixture("importer-pod", flowControllerNamespace, names.DeployName)
	require.NoError(t, c.Create(context.Background(), pod))

	// Reconcile 1: the importer is still live (the pod has not terminated), so capture must NOT be
	// attempted this pass.
	res, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)
	assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)

	deploy := &appsv1.Deployment{}
	getErr := c.Get(context.Background(), types.NamespacedName{Namespace: flowControllerNamespace, Name: names.DeployName}, deploy)
	require.Error(t, getErr)
	assert.True(t, apierrors.IsNotFound(getErr), "the importer Deployment must be deleted before capture, even though the pod is still live")

	vcrList, err := dyn.Resource(volumeCaptureRequestGVR).Namespace(flowControllerNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, vcrList.Items, "no VolumeCaptureRequest must be created while the importer pod is still live")

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
	ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, string(common.ReasonPending), ready.Reason)

	// The importer pod terminates (the kubelet finishes the unmount) -- only now is capture safe.
	require.NoError(t, c.Delete(context.Background(), pod))

	// Reconcile 2: the importer is fully stopped, so capture proceeds and creates the VolumeCaptureRequest.
	res, err = r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)
	assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)

	vcr, err := dyn.Resource(volumeCaptureRequestGVR).Namespace(flowControllerNamespace).
		Get(context.Background(), volumeCaptureRequestName(di.UID), metav1.GetOptions{})
	require.NoError(t, err, "the VolumeCaptureRequest must be created once the importer has fully stopped")
	require.NotNil(t, vcr)
}

// TestReconcile_PopulateData_CapturesInControllerNamespace guards the shape of the VolumeCaptureRequest
// the capture phase creates: it must live in the controller namespace, target the internal scratch PVC by
// name/uid with no spec.target.namespace, and be owned by the import ObjectKeeper as a controller owner
// (never blockOwnerDeletion, see keeperOwnerReference).
func TestReconcile_PopulateData_CapturesInControllerNamespace(t *testing.T) {
	t.Parallel()

	di := newPopulateDataImport("flow-imp-5")
	withUploadFinished(di)
	names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
	scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, flowControllerNamespace)

	objs := []client.Object{di, wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace), scratchPVC}
	keeper := readyObjectKeeper(objectKeeperName(di.UID), di)
	r, c, dyn := newPopulateDataFlowReconciler(t, objs, []runtime.Object{keeper})

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	vcr, err := dyn.Resource(volumeCaptureRequestGVR).Namespace(flowControllerNamespace).
		Get(context.Background(), volumeCaptureRequestName(di.UID), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, flowControllerNamespace, vcr.GetNamespace())

	target, found, err := unstructured.NestedMap(vcr.Object, "spec", "target")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, names.ImportScratchPVCName, target["name"])
	_, hasNS := target["namespace"]
	assert.False(t, hasNS, "spec.target.namespace must be omitted -- the captured PVC lives in the VCR's own namespace")

	gotPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: flowControllerNamespace, Name: names.ImportScratchPVCName}, gotPVC))
	assert.Equal(t, string(gotPVC.UID), target["uid"])

	owners := vcr.GetOwnerReferences()
	require.Len(t, owners, 1)
	assert.Equal(t, objectKeeperKind, owners[0].Kind)
	require.NotNil(t, owners[0].Controller)
	assert.True(t, *owners[0].Controller)
	assert.Nil(t, owners[0].BlockOwnerDeletion)
}

// TestReconcile_PopulateData_Idempotent drives a PopulateData import through to completion and asserts a
// second, identical reconcile issues no further writes on either the typed or the dynamic client.
func TestReconcile_PopulateData_Idempotent(t *testing.T) {
	t.Parallel()

	di := newPopulateDataImport("flow-imp-6")
	withUploadFinished(di)
	names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
	scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, flowControllerNamespace)

	artifact := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "flow-vsc-artifact-6"},
		Spec: snapv1.VolumeSnapshotContentSpec{
			DeletionPolicy: snapv1.VolumeSnapshotContentDelete,
			Driver:         "csi.example.com",
			Source:         snapv1.VolumeSnapshotContentSource{VolumeHandle: new(string)},
		},
	}

	objs := []client.Object{di, wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace), scratchPVC, artifact}
	keeper := readyObjectKeeper(objectKeeperName(di.UID), di)
	vcr := readyVolumeCaptureRequest(volumeCaptureRequestName(di.UID), flowControllerNamespace, artifact.Name)

	var typedWrites int
	countingInterceptor := interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			typedWrites++
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			typedWrites++
			return cl.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			typedWrites++
			return cl.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			typedWrites++
			return cl.Delete(ctx, obj, opts...)
		},
	}

	r, c, dyn := newPopulateDataFlowReconciler(t, objs, []runtime.Object{keeper, vcr}, countingInterceptor)

	var dynWrites int
	dyn.PrependReactor("*", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		switch action.GetVerb() {
		case "get", "list", "watch":
		default:
			dynWrites++
		}
		return false, nil, nil
	})

	res1, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
	require.NotNil(t, got.Status.Data)
	require.NotNil(t, got.Status.Data.ArtifactRef)
	assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)))

	typedWrites = 0
	dynWrites = 0
	res2, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)
	assert.Equal(t, res1, res2)
	assert.Zero(t, typedWrites, "a second reconcile against unchanged state must issue no typed-client writes")
	assert.Zero(t, dynWrites, "a second reconcile against unchanged state must issue no dynamic-client writes")
}

// TestReconcile_PopulateData_MissingInternalPVCAfterUploadIsTerminalOnlyWithoutVCR covers the one new
// terminal case in the capture phase: the internal scratch PVC being already gone is fine as long as the
// VolumeCaptureRequest it fed already exists (a prior pass captured it but failed to persist status); it
// is unrecoverable only when neither the PVC nor the VCR exists.
func TestReconcile_PopulateData_MissingInternalPVCAfterUploadIsTerminalOnlyWithoutVCR(t *testing.T) {
	t.Parallel()

	t.Run("success: PVC already gone with a Ready VCR completes normally", func(t *testing.T) {
		t.Parallel()

		di := newPopulateDataImport("flow-imp-7a")
		withUploadFinished(di)

		artifact := &snapv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "flow-vsc-artifact-7a"},
			Spec: snapv1.VolumeSnapshotContentSpec{
				DeletionPolicy: snapv1.VolumeSnapshotContentDelete,
				Driver:         "csi.example.com",
				Source:         snapv1.VolumeSnapshotContentSource{VolumeHandle: new(string)},
			},
		}
		// No scratch PVC seeded: it is already gone.
		objs := []client.Object{di, wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace), artifact}
		keeper := readyObjectKeeper(objectKeeperName(di.UID), di)
		vcr := readyVolumeCaptureRequest(volumeCaptureRequestName(di.UID), flowControllerNamespace, artifact.Name)
		r, c, _ := newPopulateDataFlowReconciler(t, objs, []runtime.Object{keeper, vcr})

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
		assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)))
		require.NotNil(t, got.Status.Data)
		require.NotNil(t, got.Status.Data.ArtifactRef)
	})

	t.Run("error: PVC gone and no VolumeCaptureRequest is terminal", func(t *testing.T) {
		t.Parallel()

		di := newPopulateDataImport("flow-imp-7b")
		withUploadFinished(di)
		names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)

		// Neither the scratch PVC nor a VolumeCaptureRequest exists.
		objs := []client.Object{di, wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace)}
		keeper := readyObjectKeeper(objectKeeperName(di.UID), di)
		r, c, _ := newPopulateDataFlowReconciler(t, objs, []runtime.Object{keeper})

		// Drive the pipeline directly first, to inspect the raw sentinel-wrapped error before Reconcile's
		// deferred block collapses a terminal error to a nil return (see TestReconcile_DI_TerminalErrorZeroResult).
		r.dataImport = di
		r.names = names
		pvcKey := types.NamespacedName{Namespace: flowControllerNamespace, Name: names.ImportScratchPVCName}
		_, err := r.captureSnapshotImportTarget(context.Background(), pvcKey)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTerminal)
		assert.ErrorIs(t, err, ErrTargetFailed)

		// The direct call above made no persisted writes (it errors before any status mutation), so
		// driving the real Reconcile next exercises the same terminal path end to end.
		res, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err, "a terminal reconcile error is recorded as phase=Failed, not surfaced as an error-requeue")
		assert.Equal(t, ctrl.Result{}, res)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
		assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)
	})
}

// TestReconcile_PopulateData_TransientErrorsPreserveReadyReason guards D5: every new transient (retryable)
// error introduced by this redesign must come out of Reconcile unwrapped (neither ErrTargetFailed nor
// ErrTerminal), so mutateReadyByErr leaves the previously persisted Ready.Reason untouched -- the same
// principle that keeps deleteDummyJobIfPending's retry gate open.
func TestReconcile_PopulateData_TransientErrorsPreserveReadyReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		importName         string
		seedUploadFinished bool
		seedReadyReason    common.ConditionReason
		buildFixture       func(names common.Names) ([]client.Object, interceptor.Funcs)
	}{
		{
			name:               "error: stopImporter Deployment delete fails",
			importName:         "flow-imp-transient-deploy",
			seedUploadFinished: true,
			seedReadyReason:    common.ReasonPending,
			buildFixture: func(names common.Names) ([]client.Object, interceptor.Funcs) {
				scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, flowControllerNamespace)
				objs := []client.Object{wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace), scratchPVC}
				injectedErr := errors.New("injected deployment delete failure")
				return objs, interceptor.Funcs{
					Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if _, ok := obj.(*appsv1.Deployment); ok {
							return injectedErr
						}
						return cl.Delete(ctx, obj, opts...)
					},
				}
			},
		},
		{
			name:               "error: importer pod List fails",
			importName:         "flow-imp-transient-podlist",
			seedUploadFinished: true,
			seedReadyReason:    common.ReasonPending,
			buildFixture: func(names common.Names) ([]client.Object, interceptor.Funcs) {
				scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, flowControllerNamespace)
				objs := []client.Object{wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace), scratchPVC}
				injectedErr := errors.New("injected pod list failure")
				return objs, interceptor.Funcs{
					List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if _, ok := list.(*corev1.PodList); ok {
							return injectedErr
						}
						return cl.List(ctx, list, opts...)
					},
				}
			},
		},
		{
			name:               "error: ensureScratchPVC Create fails",
			importName:         "flow-imp-transient-pvccreate",
			seedUploadFinished: false,
			seedReadyReason:    common.ReasonPVCCreated,
			buildFixture: func(names common.Names) ([]client.Object, interceptor.Funcs) {
				objs := []client.Object{wffcStorageClass("wffc"), wffcVolumeSnapshotClass(), exporterImageConfigMap(flowControllerNamespace)}
				injectedErr := errors.New("injected scratch PVC create failure")
				return objs, interceptor.Funcs{
					Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && pvc.Name == names.ImportScratchPVCName {
							return injectedErr
						}
						return cl.Create(ctx, obj, opts...)
					},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			di := newPopulateDataImport(tt.importName)
			if tt.seedUploadFinished {
				withUploadFinished(di)
			}
			meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
				Type:    string(common.ConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  string(tt.seedReadyReason),
				Message: "seeded: state from a prior successful reconcile",
			})

			names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
			extraObjs, ifn := tt.buildFixture(names)
			objs := append([]client.Object{di}, extraObjs...)

			r, c, _ := newPopulateDataFlowReconciler(t, objs, nil, ifn)

			_, err := r.Reconcile(context.Background(), diReq(di))
			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrTargetFailed), "a transient error must not be wrapped in ErrTargetFailed")
			assert.False(t, errors.Is(err, ErrTerminal), "a transient error must not be wrapped in ErrTerminal")

			got := &dev1alpha1.DataImport{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
			ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
			require.NotNil(t, ready)
			assert.Equal(t, string(tt.seedReadyReason), ready.Reason,
				"a transient reconcile error must not overwrite the previously persisted Ready.Reason")
		})
	}
}
