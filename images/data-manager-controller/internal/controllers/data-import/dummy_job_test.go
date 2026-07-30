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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

// dummyJobScheme extends reconcileScheme with the VolumeSnapshotClass type the PopulateData path's
// resolveSnapshotCaptureMode needs to resolve snapshot capability for the scratch PVC's StorageClass.
func dummyJobScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := reconcileScheme(t)
	require.NoError(t, snapv1.SchemeBuilder.AddToScheme(s))
	return s
}

// wffcStorageClass builds a WaitForFirstConsumer StorageClass whose volumesnapshotclass annotation
// satisfies both the dummy-Job NeedConsumer path and resolveSnapshotCaptureMode's capability check.
func wffcStorageClass(name string) *storagev1.StorageClass {
	mode := storagev1.VolumeBindingWaitForFirstConsumer
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{storageClassVSCAnnotation: "wffc-vsc"},
		},
		Provisioner:       "csi.example.com",
		VolumeBindingMode: &mode,
	}
}

// wffcVolumeSnapshotClass is the VolumeSnapshotClass referenced by wffcStorageClass's annotation; its
// Driver must match that StorageClass's Provisioner for resolveSnapshotCaptureMode to succeed.
func wffcVolumeSnapshotClass() *snapv1.VolumeSnapshotClass {
	return &snapv1.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{Name: "wffc-vsc"},
		Driver:     "csi.example.com",
	}
}

// exporterImageConfigMap seeds the ConfigMap ensureDummyJob's MakeDummyContainer reads the dummy
// container's image from. Without it the dummy Job is never created and the test would prove nothing.
func exporterImageConfigMap(namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: common.CongigMapName, Namespace: namespace},
		Data:       map[string]string{"image": "registry.example/data-exporter:test"},
	}
}

// pendingWFFCPVC builds a Pending PVC on the "wffc" StorageClass whose spec matches exactly what
// scratchPVCTemplate/EnsurePVC would derive from the DataImport specs used in this file, so EnsurePVC's
// needsPVCSpecUpdate never fires and the seeded object is reconciled as-is.
func pendingWFFCPVC(name string) *corev1.PersistentVolumeClaim {
	sc := "wffc"
	fs := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
			StorageClassName: &sc,
			VolumeMode:       &fs,
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
}

// boundWFFCPVC builds an already-Bound PVC on the "wffc" StorageClass. CheckPVCStatus returns
// TargetStatusReady from Phase==Bound alone, ignoring binding mode, so seeding Bound here means the
// dummy consumer Job is never created at all.
func boundWFFCPVC(name string) *corev1.PersistentVolumeClaim {
	pvc := pendingWFFCPVC(name)
	pvc.Status.Phase = corev1.ClaimBound
	return pvc
}

// seedPVCCreatedReadyCondition sets Ready.Reason=PVCCreated, the state a prior Pending/NeedConsumer
// reconcile leaves behind. Only realistic pre-bind staging now -- reconcileDummyJobDeletion gates on the
// Job's own existence, not on this reason.
func seedPVCCreatedReadyCondition(di *dev1alpha1.DataImport) {
	meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
		Type:    string(common.ConditionReady),
		Status:  metav1.ConditionFalse,
		Reason:  string(common.ReasonPVCCreated),
		Message: "seeded: PVC previously required a dummy consumer",
	})
}

// dummyJobKey derives the dummy consumer Job's expected namespaced name the same way Reconcile does.
func dummyJobKey(di *dev1alpha1.DataImport) types.NamespacedName {
	return types.NamespacedName{
		Namespace: di.Namespace,
		Name:      common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name).DummyJobName,
	}
}

// populateDataImport builds a PopulateData DataImport whose scratch PVC (named after the DataImport) is
// staged on the "wffc" StorageClass, matching pendingWFFCPVC's shape.
func populateDataImport(name string) *dev1alpha1.DataImport {
	return &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
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

// createPVCImportOnWFFC builds a CreatePVC DataImport whose target PVC template matches pendingWFFCPVC's
// shape, so EnsurePVC does not rewrite the seeded PVC's spec out from under the test.
func createPVCImportOnWFFC(name string) *dev1alpha1.DataImport {
	di := createPVCImport(name, "")
	sc := "wffc"
	fs := dev1alpha1.PersistentVolumeFilesystem
	di.Spec.PvcTemplate.PersistentVolumeClaimSpec = dev1alpha1.PersistentVolumeClaimSpec{
		AccessModes: []dev1alpha1.PersistentVolumeAccessMode{dev1alpha1.ReadWriteOnce},
		Resources: dev1alpha1.VolumeResourceRequirements{
			Requests: dev1alpha1.ResourceList{dev1alpha1.ResourceStorage: resource.MustParse("1Gi")},
		},
		StorageClassName: &sc,
		VolumeMode:       &fs,
	}
	return di
}

// TestReconcile_DummyJob_DeletedOnceTargetPVCBound guards both import modes: the dummy Job created
// while the PVC awaits WaitForFirstConsumer must be deleted the moment it binds, not left to its TTL.
// It drives the real Reconcile so create- and delete-time Job names share the same r.names wiring.
func TestReconcile_DummyJob_DeletedOnceTargetPVCBound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		importName         string
		buildDataImport    func(name string) *dev1alpha1.DataImport
		extraObjects       []client.Object
		assertNotCompleted bool // proves the delete happens before the UploadFinished gate, not after full completion
	}{
		{
			name:               "PopulateData: dummy Job for the scratch PVC is deleted the moment it binds",
			importName:         "populate-imp",
			buildDataImport:    populateDataImport,
			extraObjects:       []client.Object{wffcStorageClass("wffc"), wffcVolumeSnapshotClass()},
			assertNotCompleted: true,
		},
		{
			name:            "CreatePVC: dummy Job for the target PVC is deleted the moment it binds",
			importName:      "createpvc-imp",
			buildDataImport: createPVCImportOnWFFC,
			extraObjects:    []client.Object{wffcStorageClass("wffc")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			di := tt.buildDataImport(tt.importName)
			pvc := pendingWFFCPVC(tt.importName)

			objs := append([]client.Object{di, pvc, exporterImageConfigMap("d8")}, tt.extraObjects...)
			c := fake.NewClientBuilder().WithScheme(dummyJobScheme(t)).WithStatusSubresource(di).WithObjects(objs...).Build()
			r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

			jobKey := dummyJobKey(di)

			res, err := r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)
			require.NoError(t, c.Get(context.Background(), jobKey, &batchv1.Job{}),
				"the dummy consumer Job must exist while the PVC needs a WaitForFirstConsumer consumer")

			// PVC is an unconditional in-tree status-subresource kind in the fake client
			// (inTreeResourcesWithStatus); a plain Update silently drops Status -- use Status().Update.
			gotPVC := &corev1.PersistentVolumeClaim{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}, gotPVC))
			gotPVC.Status.Phase = corev1.ClaimBound
			require.NoError(t, c.Status().Update(context.Background(), gotPVC))

			_, err = r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)

			getErr := c.Get(context.Background(), jobKey, &batchv1.Job{})
			require.Error(t, getErr)
			assert.True(t, apierrors.IsNotFound(getErr),
				"the dummy consumer Job must be deleted as soon as the PVC is bound, not left to the Job TTL controller")

			if tt.assertNotCompleted {
				got := &dev1alpha1.DataImport{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
				assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)),
					"the dummy Job delete must happen at the top of the Ready branch, before the UploadFinished gate")
			}

			// A repeated reconcile must not even attempt the delete: the read-first check finds the Job
			// already gone and returns early, no new state, no error.
			_, err = r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			getErr = c.Get(context.Background(), jobKey, &batchv1.Job{})
			require.Error(t, getErr)
			assert.True(t, apierrors.IsNotFound(getErr), "a repeated no-op delete must not resurrect or error on the Job")
		})
	}
}

// TestReconcile_DummyJob_ServerReadyBeforePVCBound_StillGetsDeletedOnceBound reproduces the realistic
// PopulateData/populator ordering: the upload server pod mounts onto a separate "PvcPrime" volume, not
// the scratch PVC this reconciler's CheckPVCStatus tracks, so status.ServerState can flip to Ready (via
// the importer's own heartbeat, independent of this reconciler) WHILE the scratch PVC is still Pending.
// updateReadiness reacts to that heartbeat unconditionally and overwrites Ready.Reason to ServerReady
// on that same reconcile -- before TargetStatusReady is ever reached for the first time. By the time the
// PVC finally binds, deleteDummyJobIfPending's gate (Ready.Reason == PVCCreated) has therefore already
// been closed by an unrelated status update, and the dummy Job must not be stranded because of it.
func TestReconcile_DummyJob_ServerReadyBeforePVCBound_StillGetsDeletedOnceBound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	importName := "populate-server-ready-race"

	di := populateDataImport(importName)
	pvc := pendingWFFCPVC(importName)

	objs := []client.Object{di, pvc, exporterImageConfigMap("d8"), wffcStorageClass("wffc"), wffcVolumeSnapshotClass()}
	c := fake.NewClientBuilder().WithScheme(dummyJobScheme(t)).WithStatusSubresource(di).WithObjects(objs...).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

	jobKey := dummyJobKey(di)

	// (a) First reconcile: PVC is Pending on a WFFC StorageClass -> TargetStatusNeedConsumer -> dummy Job
	// created, Ready.Reason becomes PVCCreated.
	res, err := r.Reconcile(ctx, diReq(di))
	require.NoError(t, err)
	assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)
	require.NoError(t, c.Get(ctx, jobKey, &batchv1.Job{}),
		"the dummy consumer Job must exist while the PVC needs a WaitForFirstConsumer consumer")

	gotDI := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, gotDI))
	readyCond := meta.FindStatusCondition(gotDI.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, readyCond)
	require.Equal(t, string(common.ReasonPVCCreated), readyCond.Reason)

	// (b) Simulate the importer pod's heartbeat: the upload server on the separate PvcPrime volume
	// reports Ready while the scratch/target PVC this reconciler tracks is STILL Pending.
	gotDI.Status.ServerState = string(common.ServerStateReady)
	require.NoError(t, c.Status().Update(ctx, gotDI))

	_, err = r.Reconcile(ctx, diReq(di))
	require.NoError(t, err)

	gotDI = &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, gotDI))
	readyCond = meta.FindStatusCondition(gotDI.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, readyCond)
	require.Equal(t, string(common.ReasonServerReady), readyCond.Reason,
		"updateReadiness must have overwritten Ready.Reason to ServerReady off the importer's heartbeat "+
			"while the PVC was still Pending -- this is the race this test reproduces")

	gotPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Name}, gotPVC))
	require.Equal(t, corev1.ClaimPending, gotPVC.Status.Phase,
		"the scratch PVC must still be Pending at this point -- ServerReady came from an unrelated volume")

	// (c) Only now does the scratch PVC actually bind (in the real populator flow this happens once
	// UploadFinished triggers the PV rebind trick).
	gotPVC.Status.Phase = corev1.ClaimBound
	require.NoError(t, c.Status().Update(ctx, gotPVC))

	// (d) TargetStatusReady is reached for the very first time here. The dummy Job must be deleted
	// regardless of whichever Reason won the earlier race on Ready.
	_, err = r.Reconcile(ctx, diReq(di))
	require.NoError(t, err)

	getErr := c.Get(ctx, jobKey, &batchv1.Job{})
	require.Error(t, getErr)
	assert.True(t, apierrors.IsNotFound(getErr),
		"the dummy consumer Job must be deleted once the PVC is genuinely bound, even though Ready.Reason "+
			"had already moved on to ServerReady before TargetStatusReady was ever reached")
}

// TestReconcile_DummyJob_NeverExistedIsSafeNoOp covers TargetStatusReady reached without ever going
// through NeedConsumer (e.g. Immediate binding mode): no dummy Job is ever created, so the delete
// call at the top of the Ready branch must be a safe no-op against a Job that never existed.
func TestReconcile_DummyJob_NeverExistedIsSafeNoOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		importName      string
		buildDataImport func(name string) *dev1alpha1.DataImport
		extraObjects    []client.Object
	}{
		{
			name:            "success: PopulateData reaches Ready with no dummy Job ever created",
			importName:      "populate-immediate",
			buildDataImport: populateDataImport,
			extraObjects:    []client.Object{wffcStorageClass("wffc"), wffcVolumeSnapshotClass()},
		},
		{
			name:            "success: CreatePVC reaches Ready with no dummy Job ever created",
			importName:      "createpvc-immediate",
			buildDataImport: createPVCImportOnWFFC,
			extraObjects:    []client.Object{wffcStorageClass("wffc")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			di := tt.buildDataImport(tt.importName)
			pvc := boundWFFCPVC(tt.importName)

			objs := append([]client.Object{di, pvc, exporterImageConfigMap("d8")}, tt.extraObjects...)
			c := fake.NewClientBuilder().WithScheme(dummyJobScheme(t)).WithStatusSubresource(di).WithObjects(objs...).Build()
			r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

			jobKey := dummyJobKey(di)

			_, err := r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err, "the read-first check must skip the Delete entirely for a Job that never existed")

			getErr := c.Get(context.Background(), jobKey, &batchv1.Job{})
			require.Error(t, getErr)
			assert.True(t, apierrors.IsNotFound(getErr),
				"the dummy consumer Job must never have been created when the PVC was bound from the start")
		})
	}
}

// TestReconcile_DummyJob_DeleteFailurePropagatesForRetry guards the retry contract: a dummy-Job delete
// failure must come out of Reconcile as an error (so the standard backoff retries it, not as an
// ErrTerminal that would strand the Job in a Failed, inert object), must leave the Job present (the
// existence reconcileDummyJobDeletion re-reads next time), and once the delete eventually succeeds must
// not be retried again. It covers both modes since handleTargetStatus (PopulateData) and
// handlePVCImportStatus (CreatePVC) each call reconcileDummyJobDeletion independently.
func TestReconcile_DummyJob_DeleteFailurePropagatesForRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		importName       string
		scheme           func(t *testing.T) *runtime.Scheme
		buildObjs        func(importName string) (di *dev1alpha1.DataImport, extra []client.Object)
		assertAfterRetry func(t *testing.T, c client.Client, di *dev1alpha1.DataImport, res ctrl.Result)
	}{
		{
			name:       "CreatePVC: import reaches Completed once the retried delete succeeds",
			importName: "imp-delete-retry-createpvc",
			scheme:     reconcileScheme,
			buildObjs: func(importName string) (*dev1alpha1.DataImport, []client.Object) {
				di := createPVCImport(importName, common.ServerStateFinished)
				// Realistic pre-bind staging: a prior reconcile already observed NeedConsumer/Pending
				// (Reason=PVCCreated) before the PVC bound. reconcileDummyJobDeletion no longer reads this
				// reason -- the preseeded dummy Job below is what the delete call (and injected failure)
				// actually gates on.
				seedPVCCreatedReadyCondition(di)
				pvc := boundImportPVC(importName)
				return di, []client.Object{pvc}
			},
			assertAfterRetry: func(t *testing.T, c client.Client, di *dev1alpha1.DataImport, res ctrl.Result) {
				t.Helper()
				got := &dev1alpha1.DataImport{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
				assert.Equal(t, string(common.PhaseCompleted), got.Status.Phase)
				require.NotNil(t, got.Status.CompletionTimestamp)
				assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)))
				assert.Zero(t, res.RequeueAfter)
			},
		},
		{
			name:       "PopulateData: import progresses to awaiting-upload once the retried delete succeeds",
			importName: "imp-delete-retry-populate",
			scheme:     dummyJobScheme,
			buildObjs: func(importName string) (*dev1alpha1.DataImport, []client.Object) {
				di := populateDataImport(importName)
				seedPVCCreatedReadyCondition(di)
				pvc := boundWFFCPVC(importName)
				return di, []client.Object{
					pvc, exporterImageConfigMap("d8"), wffcStorageClass("wffc"), wffcVolumeSnapshotClass(),
				}
			},
			assertAfterRetry: func(t *testing.T, c client.Client, di *dev1alpha1.DataImport, res ctrl.Result) {
				t.Helper()
				got := &dev1alpha1.DataImport{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
				// UploadFinished was never set, so the import is stuck awaiting the upload, not Completed.
				assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)))
				assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)
				readyCond := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
				require.NotNil(t, readyCond)
				assert.Equal(t, string(common.ReasonPending), readyCond.Reason)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := tt.scheme(t)
			di, extra := tt.buildObjs(tt.importName)
			jobKey := dummyJobKey(di)

			preseededJob := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: jobKey.Namespace},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: "dummy-consumer", Image: "registry.example/data-exporter:test"}},
						},
					},
				},
			}

			injectedErr := errors.New("injected dummy Job delete failure")
			// failDelete and deleteAttempts are plain locals: Reconcile and the fake client both run
			// synchronously within this subtest, so there is no concurrent access to guard against.
			failDelete := true
			deleteAttempts := 0

			objs := append([]client.Object{di, preseededJob}, extra...)
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(di).WithObjects(objs...).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if job, ok := obj.(*batchv1.Job); ok && job.Name == jobKey.Name {
							deleteAttempts++
							if failDelete {
								return injectedErr
							}
						}
						// deletePublish also deletes Service/Ingress on this path; only the dummy Job delete
						// is ever intercepted above.
						return cl.Delete(ctx, obj, opts...)
					},
				}).Build()
			r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

			// (a) First reconcile: the delete fails and must propagate out of Reconcile for retry.
			res, err := r.Reconcile(context.Background(), diReq(di))
			require.Error(t, err)
			assert.ErrorIs(t, err, injectedErr)
			assert.NotErrorIs(t, err, ErrTerminal,
				"a transient dummy-Job delete failure must stay retryable; ErrTerminal would drive phase=Failed, "+
					"make the reconcile body inert and strand the Job forever")
			assert.Equal(t, ctrl.Result{}, res)
			assert.Equal(t, 1, deleteAttempts)
			require.NoError(t, c.Get(context.Background(), jobKey, &batchv1.Job{}),
				"the dummy Job must remain present when its delete fails -- its existence is what the next "+
					"reconcile re-reads to retry the delete")

			got := &dev1alpha1.DataImport{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
			assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)))
			assert.Nil(t, got.Status.CompletionTimestamp)
			assert.NotEqual(t, string(common.PhaseFailed), got.Status.Phase)

			// (b) Second reconcile: the delete now succeeds and the import makes progress.
			failDelete = false
			res, err = r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			assert.Equal(t, 2, deleteAttempts)
			getErr := c.Get(context.Background(), jobKey, &batchv1.Job{})
			require.Error(t, getErr)
			assert.True(t, apierrors.IsNotFound(getErr))
			tt.assertAfterRetry(t, c, di, res)

			// (c) Third reconcile: idempotent -- the Job is gone, so the read-first check skips the
			// Delete entirely: no write call on unchanged state.
			_, err = r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			assert.Equal(t, 2, deleteAttempts)
			getErr = c.Get(context.Background(), jobKey, &batchv1.Job{})
			require.Error(t, getErr)
			assert.True(t, apierrors.IsNotFound(getErr))
		})
	}
}

// TestReconcile_DummyJob_GetFailurePropagatesForRetry guards the read-first check reconcileDummyJobDeletion
// added on top of the old delete-only logic: a transient failure reading the dummy Job must come out of
// Reconcile as a retryable error (never ErrTerminal, which would strand the object in phase=Failed), must
// leave the Job untouched, and once the read succeeds the delete it was gating must still go through --
// a Get failure must never be silently treated as "the Job doesn't exist" and skip cleanup.
func TestReconcile_DummyJob_GetFailurePropagatesForRetry(t *testing.T) {
	t.Parallel()

	di := populateDataImport("imp-get-retry")
	pvc := boundWFFCPVC("imp-get-retry")
	jobKey := dummyJobKey(di)
	preseededJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: jobKey.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "dummy-consumer", Image: "registry.example/data-exporter:test"}},
				},
			},
		},
	}

	injectedErr := errors.New("injected dummy Job get failure")
	// failGet is a plain local: Reconcile and the fake client both run synchronously in this test, so
	// there is no concurrent access to guard against.
	failGet := true

	objs := []client.Object{di, preseededJob, pvc, exporterImageConfigMap("d8"), wffcStorageClass("wffc"), wffcVolumeSnapshotClass()}
	c := fake.NewClientBuilder().WithScheme(dummyJobScheme(t)).WithStatusSubresource(di).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*batchv1.Job); ok && key.Name == jobKey.Name && failGet {
					return injectedErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

	// (a) First reconcile: reading the Job fails and must propagate out of Reconcile for retry.
	res, err := r.Reconcile(context.Background(), diReq(di))
	require.Error(t, err)
	assert.ErrorIs(t, err, injectedErr)
	assert.NotErrorIs(t, err, ErrTerminal,
		"a transient dummy-Job read failure must stay retryable; ErrTerminal would drive phase=Failed and "+
			"make the reconcile body inert")
	assert.Equal(t, ctrl.Result{}, res)

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
	assert.NotEqual(t, string(common.PhaseFailed), got.Status.Phase)

	// Stop intercepting before the verification Get below -- the interceptor has no way to distinguish
	// "the reconciler is reading the Job" from "the test is reading the Job", and only the former should
	// ever fail here.
	failGet = false
	require.NoError(t, c.Get(context.Background(), jobKey, &batchv1.Job{}),
		"the dummy Job must be untouched while its own read is failing")

	// (b) Second reconcile: the read now succeeds, and the delete it was gating actually happens.
	_, err = r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)
	getErr := c.Get(context.Background(), jobKey, &batchv1.Job{})
	require.Error(t, getErr)
	assert.True(t, apierrors.IsNotFound(getErr),
		"once the Job read succeeds, the delete deferred by the earlier Get failure must go through")
}

// TestReconcile_DummyJob_SkipsDeleteWhileJobTerminating guards the job.DeletionTimestamp != nil branch of
// reconcileDummyJobDeletion: once a Delete has been issued and foreground propagation is still draining the
// dummy Pod (the Job carries a finalizer and so is marked for deletion but not yet gone), a repeated
// reconcile must not re-issue Delete -- that would be a mutating call on unchanged state and cannot unstick
// the propagation anyway.
func TestReconcile_DummyJob_SkipsDeleteWhileJobTerminating(t *testing.T) {
	t.Parallel()

	di := populateDataImport("imp-terminating-job")
	pvc := boundWFFCPVC("imp-terminating-job")
	jobKey := dummyJobKey(di)
	preseededJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:       jobKey.Name,
			Namespace:  jobKey.Namespace,
			Finalizers: []string{"test.example/hold"},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "dummy-consumer", Image: "registry.example/data-exporter:test"}},
				},
			},
		},
	}

	deleteAttempts := 0
	objs := []client.Object{di, preseededJob, pvc, exporterImageConfigMap("d8"), wffcStorageClass("wffc"), wffcVolumeSnapshotClass()}
	c := fake.NewClientBuilder().WithScheme(dummyJobScheme(t)).WithStatusSubresource(di).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if job, ok := obj.(*batchv1.Job); ok && job.Name == jobKey.Name {
					deleteAttempts++
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: "d8"}, Now: func() metav1.Time { return fixedNow }}

	// (a) First reconcile: the Job has a finalizer, so Delete marks it terminating instead of removing it
	// (confirmed against the vendored fake client: a finalizer-bearing object gets deletionTimestamp
	// stamped via an Update, not actually removed, on Delete).
	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)
	assert.Equal(t, 1, deleteAttempts)
	gotJob := &batchv1.Job{}
	require.NoError(t, c.Get(context.Background(), jobKey, gotJob),
		"a finalizer-bearing Job stays present, only marked for deletion, until the finalizer is removed")
	require.NotNil(t, gotJob.DeletionTimestamp, "Delete on a finalizer-bearing object must stamp deletionTimestamp")

	// (b) Second reconcile: the Job is still terminating -- must not re-issue Delete.
	_, err = r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)
	assert.Equal(t, 1, deleteAttempts,
		"reconcileDummyJobDeletion must skip a Job that already has deletionTimestamp set, not re-issue Delete")
}
