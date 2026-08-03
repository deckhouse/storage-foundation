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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// The restart matrix. The controller stores no step number anywhere: every stage re-derives from the
// world whether it has already happened. That is what makes an arbitrary restart safe, and it is also
// what can go silently wrong — a stage that reads the world too generously redoes work that must happen
// once, and one that reads it too eagerly skips a check that must happen every time.
//
// Each case here restarts the controller from one persisted checkpoint and asserts both halves: it
// continues from the right place, and it does not repeat what the checkpoint says is done.

const restartPVName = "test-pv"

// restartWorld is one reconcile pass over a checkpoint, with everything the assertions need to see: what
// the controller wrote, and what the objects look like afterwards.
type restartWorld struct {
	t      *testing.T
	client client.Client
	log    *mutationLog
	result ctrl.Result
	err    error
}

// resumeFrom starts the controller against a world frozen at some checkpoint. The context deadline stands
// in for the next restart: the readiness poll waits for an exporter pod that no fake client will ever
// start, and how long it waits is not what any of these cases are about.
func resumeFrom(t *testing.T, objs ...client.Object) *restartWorld {
	t.Helper()

	recorded := &mutationLog{}
	// The fake client leaves a created object without a UID, which the API server never does. Identity is
	// the whole subject here: an export claim created without one would make every later comparison
	// vacuous.
	intercept := recorded.interceptors()
	create := intercept.Create
	intercept.Create = func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetUID() == "" {
			obj.SetUID(types.UID("uid-of-" + obj.GetName()))
		}
		return create(ctx, cl, obj, opts...)
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(setupTestScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		WithInterceptorFuncs(intercept).
		Build()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	result, err := createTestReconciler(fakeClient, fakeClient, createTestConfig()).Reconcile(ctx, deRequest)
	return &restartWorld{t: t, client: fakeClient, log: recorded, result: result, err: err}
}

func (w *restartWorld) export() *dev1alpha1.DataExport {
	w.t.Helper()
	dataExport := &dev1alpha1.DataExport{}
	require.NoError(w.t, w.client.Get(context.Background(), deRequest.NamespacedName, dataExport))
	return dataExport
}

func (w *restartWorld) volume() *corev1.PersistentVolume {
	w.t.Helper()
	pv := &corev1.PersistentVolume{}
	require.NoError(w.t, w.client.Get(context.Background(), types.NamespacedName{Name: restartPVName}, pv))
	return pv
}

func (w *restartWorld) claim(namespace, name string) *corev1.PersistentVolumeClaim {
	w.t.Helper()
	claim := &corev1.PersistentVolumeClaim{}
	err := w.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, claim)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(w.t, err)
	return claim
}

func (w *restartWorld) deploymentExists() bool {
	w.t.Helper()
	err := w.client.Get(context.Background(),
		types.NamespacedName{Namespace: testExportPVCNamespace, Name: testNames.DeployName}, &appsv1.Deployment{})
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(w.t, err)
	return true
}

// did reports whether the pass performed a write matching the description, e.g. "patch test-pv".
func (w *restartWorld) did(write string) bool {
	for _, recorded := range w.log.writes {
		if recorded == write {
			return true
		}
	}
	return false
}

// changes are the writes that could alter the cluster. The teardown deletes the public Service and
// Ingress without looking first, so those two attempts are recorded on worlds that never had them; they
// say nothing about whether anything was done.
func (w *restartWorld) changes() []string {
	var changes []string
	for _, recorded := range w.log.writes {
		if recorded == "delete "+testNames.HeadlessServiceName || recorded == "delete "+testNames.IngressResourceName {
			continue
		}
		changes = append(changes, recorded)
	}
	return changes
}

func (w *restartWorld) readyReason() string {
	w.t.Helper()
	ready := meta.FindStatusCondition(w.export().Status.Conditions, string(common.ConditionReady))
	require.NotNil(w.t, ready)
	return ready.Reason
}

// --- the world at each checkpoint ---------------------------------------------------------------

func restartExport() *dev1alpha1.DataExport {
	return &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{
			Name: dataExportName, Namespace: dataExportNamespace, UID: testDataExportUID,
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec: dev1alpha1.DataExportSpec{
			TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindPVC, Name: testUserPVCName},
			Ttl:       "1h",
		},
		Status: dev1alpha1.DataExportImportStatus{
			Phase: string(common.PhasePending),
			Conditions: []metav1.Condition{{
				Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
				Reason: string(common.ReasonPending), LastTransitionTime: metav1.NewTime(time.Now()),
			}},
		},
	}
}

func recordedTakeover() *dev1alpha1.RecoveryStatus {
	return &dev1alpha1.RecoveryStatus{
		SourcePVCUID: string(testUserPVCUID),
		ExportPVCUID: string(testExportPVCUID),
		PVName:       restartPVName,
		PVUID:        string(testPVUID),
	}
}

// ownerClaim is the user's claim as it stands before the export touches it.
func ownerClaim() *corev1.PersistentVolumeClaim {
	volumeMode := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testUserPVCName, Namespace: dataExportNamespace, UID: testUserPVCUID, ResourceVersion: "1",
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: restartPVName, VolumeMode: &volumeMode},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// heldClaim is the same claim once the export has marked it: the annotation and the finalizer are what
// keep it alive while its volume is in our hands.
func heldClaim() *corev1.PersistentVolumeClaim {
	claim := ownerClaim()
	claim.Annotations = map[string]string{DataExportInProgressKey: "true"}
	claim.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
	return claim
}

// strandedClaim is that claim after the volume was taken from it: the storage layer reports it Lost until
// a binder puts the two back together, which is what B4 waits for.
func strandedClaim() *corev1.PersistentVolumeClaim {
	claim := heldClaim()
	claim.Status.Phase = corev1.ClaimLost
	return claim
}

func ownerVolume() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: restartPVName, UID: testPVUID, ResourceVersion: "1"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			ClaimRef: &corev1.ObjectReference{
				Namespace: dataExportNamespace, Name: testUserPVCName, UID: testUserPVCUID,
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
}

// takenOverVolume is the volume after the rebind: protected by Retain, held by the export claim and
// carrying the identity of the takeover.
func takenOverVolume() *corev1.PersistentVolume {
	pv := ownerVolume()
	pv.Annotations = withUIDAnnotations(testDataExportUID, testUserPVCUID)
	pv.Labels = map[string]string{dev1alpha1.LabelPVDataExporter: "true"}
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: testExportPVCNamespace, Name: testNames.ExportPVCName, UID: testExportPVCUID,
	}
	return pv
}

func exportClaim() *corev1.PersistentVolumeClaim {
	volumeMode := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNames.ExportPVCName, Namespace: testExportPVCNamespace, UID: testExportPVCUID, ResourceVersion: "1",
			Labels:      map[string]string{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue},
			Annotations: map[string]string{dev1alpha1.AnnotationDataExportUIDKey: string(testDataExportUID)},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: restartPVName, VolumeMode: &volumeMode},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func exporterDeployment() *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNames.DeployName, Namespace: testExportPVCNamespace,
			Labels: map[string]string{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue},
			Annotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: dataExportNamespace,
				dev1alpha1.AnnotationStorageManagerNameKey:      dataExportName,
			},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

func exporterImageConfig() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: common.CongigMapName, Namespace: testExportPVCNamespace},
		Data:       map[string]string{"image": "registry.example/data-exporter:test"},
	}
}

// owingRecovery is an export that has already been found broken: the failure is on the object and the
// discriminator says the volume is still owed back. Every recovery checkpoint starts from this.
func owingRecovery() *dev1alpha1.DataExport {
	dataExport := restartExport()
	dataExport.Status.Recovery = recordedTakeover()
	dataExport.Status.CleanupReason = string(common.CleanupReasonExportPVCPostRebindLost)
	dataExport.Status.Conditions = []metav1.Condition{{
		Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
		Reason: string(common.ReasonManagedResourceLost), LastTransitionTime: metav1.NewTime(time.Now()),
	}}
	return dataExport
}

// returnedVolume is the volume at the end of a recovery: back with its owner, unmarked and under its
// original reclaim policy.
func returnedVolume() *corev1.PersistentVolume {
	pv := ownerVolume()
	pv.Spec.ClaimRef.ResourceVersion = "1"
	return pv
}

func TestRestartMatrix(t *testing.T) {
	for _, tt := range []struct {
		// state and checkpoint name the row of the restart matrix (§9 of the design) this case restarts
		// from.
		state      string
		checkpoint string
		world      func() []client.Object
		resumes    func(t *testing.T, w *restartWorld)
	}{
		{
			state:      "NoExport",
			checkpoint: "nothing has been done yet",
			world: func() []client.Object {
				return []client.Object{restartExport(), ownerVolume(), ownerClaim()}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				assert.True(t, w.did("create "+testNames.ExportPVCName), "the export claim is provisioned")

				pv := w.volume()
				require.NotNil(t, pv.Spec.ClaimRef)
				assert.Equal(t, testNames.ExportPVCName, pv.Spec.ClaimRef.Name, "and the volume is taken over for it")
				assert.Equal(t, corev1.PersistentVolumeReclaimRetain, pv.Spec.PersistentVolumeReclaimPolicy)

				created := w.claim(testExportPVCNamespace, testNames.ExportPVCName)
				require.NotNil(t, created)
				recorded := w.export().Status.Recovery
				require.NotNil(t, recorded, "the identity is recorded before the volume changes hands, not after")
				assert.Equal(t, &dev1alpha1.RecoveryStatus{
					SourcePVCUID: string(testUserPVCUID),
					ExportPVCUID: string(created.UID),
					PVName:       restartPVName,
					PVUID:        string(testPVUID),
				}, recorded)
			},
		},
		{
			state:      "PreRebind",
			checkpoint: "export claim created, volume not yet taken over",
			world: func() []client.Object {
				return []client.Object{restartExport(), ownerVolume(), ownerClaim(), exportClaim()}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				assert.False(t, w.did("create "+testNames.ExportPVCName),
					"the claim that already exists is the one to use; a second one could never hold the volume")

				pv := w.volume()
				require.NotNil(t, pv.Spec.ClaimRef)
				assert.Equal(t, testNames.ExportPVCName, pv.Spec.ClaimRef.Name, "the rebind is where this resumes")
				assert.Equal(t, corev1.PersistentVolumeReclaimRetain, pv.Spec.PersistentVolumeReclaimPolicy)
				assert.Equal(t, recordedTakeover(), w.export().Status.Recovery)
			},
		},
		{
			state:      "PostRebind",
			checkpoint: "volume taken over, no exporter yet",
			world: func() []client.Object {
				dataExport := restartExport()
				dataExport.Status.Recovery = recordedTakeover()
				return []client.Object{
					dataExport, takenOverVolume(), heldClaim(), exportClaim(), exporterImageConfig(),
				}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				assert.True(t, w.deploymentExists(), "what is missing is the exporter, so that is what gets built")
				assert.False(t, w.did("patch "+restartPVName),
					"the volume has already changed hands; taking it over twice is not idempotent")
			},
		},
		{
			state:      "Serving",
			checkpoint: "the exporter reports it is serving",
			world: func() []client.Object {
				dataExport := restartExport()
				dataExport.Status.Recovery = recordedTakeover()
				dataExport.Status.ServerState = string(common.ServerStateReady)
				dataExport.Status.Phase = string(common.PhaseReady)
				dataExport.Status.Conditions = []metav1.Condition{{
					Type: string(common.ConditionReady), Status: metav1.ConditionTrue,
					Reason: string(common.ReasonServerReady), LastTransitionTime: metav1.NewTime(time.Now()),
				}}
				return []client.Object{
					dataExport, takenOverVolume(), heldClaim(), exportClaim(), exporterDeployment(),
				}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				require.NoError(t, w.err, "a serving export reconciles cleanly; an error here means another branch was taken")
				assert.Empty(t, w.changes(), "a serving export is steady state: nothing is provisioned again")
				assert.Equal(t, string(common.PhaseReady), w.export().Status.Phase)
			},
		},
		{
			state:      "RecoveryRequired",
			checkpoint: "failure recorded, nothing torn down yet",
			world: func() []client.Object {
				return []client.Object{
					owingRecovery(), takenOverVolume(), strandedClaim(), exportClaim(), exporterDeployment(),
				}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				require.NoError(t, w.err)
				assert.True(t, w.did("delete "+testNames.DeployName),
					"the teardown starts where it always starts: nothing may still be using the volume")
				assert.False(t, w.deploymentExists())
				assert.Nil(t, w.claim(testExportPVCNamespace, testNames.ExportPVCName), "the claim holding it goes next")
				assert.Equal(t, testUserPVCName, w.volume().Spec.ClaimRef.Name, "and the volume goes home")
				assert.Equal(t, string(common.CleanupReasonExportPVCPostRebindLost), w.export().Status.CleanupReason,
					"the binder has not confirmed the binding, so the recovery is still owed")
			},
		},
		{
			state:      "RecoveryRequired",
			checkpoint: "exporter already gone, export claim still there",
			world: func() []client.Object {
				return []client.Object{
					owingRecovery(), takenOverVolume(), strandedClaim(), exportClaim(),
					consumerPod("still-mounted", corev1.PodRunning, testNames.ExportPVCName),
				}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				// The Deployment being gone is exactly what makes this dangerous: a resume that reads
				// "step 1 done" from the world would walk past the barriers step 1 exists to reach.
				require.NoError(t, w.err, "a barrier is a wait, not a failure")
				assert.NotNil(t, w.claim(testExportPVCNamespace, testNames.ExportPVCName),
					"a pod still has the volume mounted; the claim holding it may not be deleted")
				assert.Equal(t, testNames.ExportPVCName, w.volume().Spec.ClaimRef.Name)
				assert.Equal(t, string(common.ReasonCleanupBlocked), w.readyReason())
				assert.Equal(t, ctrl.Result{RequeueAfter: dataExportRequeueInterval}, w.result)
			},
		},
		{
			state:      "RecoveryRequired",
			checkpoint: "export claim deleted, volume still bound to it",
			world: func() []client.Object {
				return []client.Object{owingRecovery(), takenOverVolume(), strandedClaim()}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				require.NoError(t, w.err)
				pv := w.volume()
				require.NotNil(t, pv.Spec.ClaimRef)
				assert.Equal(t, testUserPVCName, pv.Spec.ClaimRef.Name,
					"nothing holds the volume any more, so it goes back to its owner")
				assert.Equal(t, corev1.PersistentVolumeReclaimRetain, pv.Spec.PersistentVolumeReclaimPolicy,
					"and stays protected until the binding is confirmed")
			},
		},
		{
			state:      "RecoveryRequired",
			checkpoint: "volume rebound, still under Retain",
			world: func() []client.Object {
				pv := takenOverVolume()
				pv.Spec.ClaimRef = &corev1.ObjectReference{
					Namespace: dataExportNamespace, Name: testUserPVCName, UID: testUserPVCUID,
				}
				return []client.Object{owingRecovery(), pv, heldClaim()}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				require.NoError(t, w.err)
				pv := w.volume()
				assert.Equal(t, corev1.PersistentVolumeReclaimDelete, pv.Spec.PersistentVolumeReclaimPolicy,
					"the binding is confirmed, so the volume is put back the way it was found")
				assertPVExportMetadataRemoved(t, pv)
			},
		},
		{
			state:      "RecoveryRequired",
			checkpoint: "volume fully restored, source claim still held",
			world: func() []client.Object {
				return []client.Object{owingRecovery(), returnedVolume(), heldClaim()}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				require.NoError(t, w.err)
				assert.False(t, w.did("patch "+restartPVName),
					"an unmarked volume is one that was already given back; restoring it again would condemn a healthy volume")

				claim := w.claim(dataExportNamespace, testUserPVCName)
				require.NotNil(t, claim)
				assert.Empty(t, claim.Finalizers, "the claim is released last, and this is what is left to do")
				assert.NotContains(t, claim.Annotations, DataExportInProgressKey)
			},
		},
		{
			state:      "RecoveryRequired",
			checkpoint: "everything undone, discriminator still set",
			world: func() []client.Object {
				return []client.Object{owingRecovery(), returnedVolume(), ownerClaim()}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				require.NoError(t, w.err)
				assert.Empty(t, w.changes(), "there is nothing left to undo")

				dataExport := w.export()
				assert.Empty(t, dataExport.Status.CleanupReason, "the recovery is finished, so it is no longer owed")
				assert.Nil(t, dataExport.Status.Recovery)
				assert.Equal(t, string(common.PhaseFailed), dataExport.Status.Phase)
				assert.Equal(t, string(common.ReasonManagedResourceLost), w.readyReason(),
					"the object settles on what went wrong, not on the last barrier it waited at")
				assert.Len(t, w.log.statusWrites, 1, "one pass, one status write")
			},
		},
		{
			state:      "FailedTerminal",
			checkpoint: "settled after recovery, awaiting deletion",
			world: func() []client.Object {
				dataExport := restartExport()
				dataExport.Status.Phase = string(common.PhaseFailed)
				settled := metav1.NewTime(time.Now().Add(-time.Hour))
				dataExport.Status.CompletionTimestamp = &settled
				dataExport.Status.Conditions = []metav1.Condition{{
					Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
					Reason: string(common.ReasonManagedResourceLost), LastTransitionTime: metav1.NewTime(time.Now()),
				}}
				return []client.Object{dataExport, returnedVolume(), ownerClaim()}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				require.NoError(t, w.err)
				assert.Empty(t, w.log.writes, "a settled object is not re-examined")
				assert.Empty(t, w.log.statusWrites)
				assert.Equal(t, ctrl.Result{}, w.result)

				dataExport := w.export()
				assert.Equal(t, string(common.PhaseFailed), dataExport.Status.Phase)
				assert.True(t, common.ContainsString(dataExport.Finalizers, dev1alpha1.StorageManagerFinalizerName),
					"the finalizer waits for the DELETE that has not come yet")
			},
		},
		{
			state:      "Deleting",
			checkpoint: "deletion requested with the volume already home",
			world: func() []client.Object {
				dataExport := restartExport()
				deleted := metav1.NewTime(time.Now().Add(-time.Minute))
				dataExport.DeletionTimestamp = &deleted
				dataExport.Status.Recovery = recordedTakeover()
				return []client.Object{dataExport, returnedVolume(), ownerClaim()}
			},
			resumes: func(t *testing.T, w *restartWorld) {
				require.NoError(t, w.err)
				err := w.client.Get(context.Background(), deRequest.NamespacedName, &dev1alpha1.DataExport{})
				assert.True(t, apierrors.IsNotFound(err),
					"the teardown has nothing left to do, so the object is finally released")
			},
		},
	} {
		t.Run(tt.state+": "+tt.checkpoint, func(t *testing.T) {
			w := resumeFrom(t, tt.world()...)
			tt.resumes(t, w)
		})
	}
}

// An export finds its claim by a name it can compute from its own namespace and name, so finding one
// proves nothing about who created it. Before the rebind that is the only thing it has: the takeover is
// not recorded yet and the volume is still the user's. Using a claim on the strength of its name alone is
// how an unproven object becomes a proven one — its UID is written into status.recovery on the next line
// and every later check then compares against a stranger.
func TestPreRebind_ClaimIsUsedOnlyWhenItsOriginIsProven(t *testing.T) {
	strangerUID := types.UID("uid-of-another-data-export")

	claimOfAnotherExport := func() *corev1.PersistentVolumeClaim {
		claim := exportClaim()
		claim.UID = "uid-of-a-stranger"
		claim.Annotations[dev1alpha1.AnnotationDataExportUIDKey] = string(strangerUID)
		return claim
	}
	unmarkedClaim := func() *corev1.PersistentVolumeClaim {
		claim := exportClaim()
		delete(claim.Annotations, dev1alpha1.AnnotationDataExportUIDKey)
		return claim
	}
	// refusedToAdopt is the whole point of refusing: the volume stays with its owner and the claim that
	// could not be proven is left exactly as it was found.
	refusedToAdopt := func(t *testing.T, w *restartWorld) {
		t.Helper()
		require.NoError(t, w.err, "a name conflict is not this object's failure to report as an error")
		assert.Empty(t, w.changes(), "an unproven claim is neither used, mutated nor deleted")

		pv := w.volume()
		require.NotNil(t, pv.Spec.ClaimRef)
		assert.Equal(t, testUserPVCName, pv.Spec.ClaimRef.Name, "the takeover never starts")
		assert.Equal(t, corev1.PersistentVolumeReclaimDelete, pv.Spec.PersistentVolumeReclaimPolicy)

		dataExport := w.export()
		assert.Nil(t, dataExport.Status.Recovery, "nothing may be recorded as identity that was not proven")
		assert.Empty(t, dataExport.Status.CleanupReason,
			"no volume changed hands, so no recovery is owed; a discriminator here would send the teardown after someone else's claim")
		assert.Equal(t, string(common.ReasonCleanupBlocked), w.readyReason())
		assert.Equal(t, string(common.PhasePending), dataExport.Status.Phase, "the conflict is curable from outside, so this is not terminal")
		assert.Equal(t, ctrl.Result{RequeueAfter: dataExportRequeueInterval}, w.result,
			"and curable from outside means someone else's claim disappearing, which nothing here watches")
	}

	for _, tt := range []struct {
		claim   string
		world   func() []client.Object
		asserts func(t *testing.T, w *restartWorld)
	}{
		{
			claim: "carries this export's UID",
			world: func() []client.Object {
				return []client.Object{restartExport(), ownerVolume(), ownerClaim(), exportClaim()}
			},
			asserts: func(t *testing.T, w *restartWorld) {
				assert.False(t, w.did("create "+testNames.ExportPVCName), "the proven claim is the one to use")
				assert.Equal(t, testNames.ExportPVCName, w.volume().Spec.ClaimRef.Name, "so the volume is taken over for it")
				assert.Equal(t, recordedTakeover(), w.export().Status.Recovery, "and only now is its UID identity")
			},
		},
		{
			claim: "carries another export's UID",
			world: func() []client.Object {
				return []client.Object{restartExport(), ownerVolume(), ownerClaim(), claimOfAnotherExport()}
			},
			asserts: refusedToAdopt,
		},
		{
			claim: "carries no marker at all",
			world: func() []client.Object {
				return []client.Object{restartExport(), ownerVolume(), ownerClaim(), unmarkedClaim()}
			},
			asserts: refusedToAdopt,
		},
		{
			// The same object seen from the other side: the export that made the leftover claim is gone
			// and a new one has been created under the same namespace and name. Everything a name-based
			// check can see matches; only the parent UID differs.
			claim: "was left behind by a previous export of the same name",
			world: func() []client.Object {
				recreated := restartExport()
				recreated.UID = "uid-of-the-recreated-data-export"
				leftover := exportClaim()
				leftover.UID = "uid-of-the-previous-claim"
				return []client.Object{recreated, ownerVolume(), ownerClaim(), leftover}
			},
			asserts: refusedToAdopt,
		},
		{
			// Neither the marker nor the recorded identity existed when this export started, so it has
			// neither. The volume vouches for the claim instead: it is in our hands and bound to that
			// very claim, which is the same evidence the recovery path acts on. Without this an upgrade
			// would strand every export it caught mid-life.
			claim: "predates the marker but holds the volume this export took over",
			world: func() []client.Object {
				legacyVolume := takenOverVolume()
				delete(legacyVolume.Annotations, dev1alpha1.AnnotationDataExportUIDKey)
				delete(legacyVolume.Annotations, dev1alpha1.AnnotationUserPVCUIDKey)
				return []client.Object{
					restartExport(), legacyVolume, heldClaim(), unmarkedClaim(), exporterImageConfig(),
				}
			},
			asserts: func(t *testing.T, w *restartWorld) {
				assert.True(t, w.deploymentExists(), "the export continues where it left off")
				assert.NotEqual(t, string(common.ReasonCleanupBlocked), w.readyReason())
			},
		},
	} {
		t.Run("the claim "+tt.claim, func(t *testing.T) {
			w := resumeFrom(t, tt.world()...)
			tt.asserts(t, w)
		})
	}
}

// TestPreRebind_CreatedClaimCarriesItsOrigin covers the other half: the check above is only worth having
// if the claim the controller creates is one it will later be able to prove.
func TestPreRebind_CreatedClaimCarriesItsOrigin(t *testing.T) {
	w := resumeFrom(t, restartExport(), ownerVolume(), ownerClaim())

	created := w.claim(testExportPVCNamespace, testNames.ExportPVCName)
	require.NotNil(t, created)
	assert.Equal(t, string(testDataExportUID), created.Annotations[dev1alpha1.AnnotationDataExportUIDKey],
		"the claim is stamped with its parent at creation, which is the only moment its origin is known first-hand")
}

// TestRestart_ProvisioningNeverRetakesTheVolume states the invariant the provisioning rows above each
// check one instance of: however many times the controller restarts mid-provisioning, the user's volume
// is taken over once. A second takeover would re-stamp the identity of a volume already in our hands and
// destroy the record of who it was borrowed from.
func TestRestart_ProvisioningNeverRetakesTheVolume(t *testing.T) {
	dataExport := restartExport()
	dataExport.Status.Recovery = recordedTakeover()

	w := resumeFrom(t, dataExport, takenOverVolume(), heldClaim(), exportClaim(), exporterImageConfig())

	for _, write := range w.log.writes {
		assert.False(t, strings.HasPrefix(write, "patch "+restartPVName), "unexpected write to the volume: %s", write)
	}
	assert.Equal(t, recordedTakeover(), w.export().Status.Recovery, "the record is write-once")
}
