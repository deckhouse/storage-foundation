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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

func recoverySchemeWithStorage(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := setupTestScheme()
	require.NoError(t, storagev1.SchemeBuilder.AddToScheme(scheme))
	return scheme
}

// consumerPod is a pod holding the export claim, which is what B1 looks for regardless of its phase.
func consumerPod(name string, phase corev1.PodPhase, claimName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testExportPVCNamespace},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
			},
		}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func volumeAttachment(name, pvName string, attached bool) *storagev1.VolumeAttachment {
	return &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: "test.csi", NodeName: "node-1",
			Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: attached},
	}
}

// TestBarrierNoConsumerPods pins the strict reading of B1: only the pod being gone proves the volume is
// unmounted. A Succeeded or Failed pod says the containers stopped, not that the kubelet finished
// tearing the volume down, and for non-attachable volumes there is no second signal to fall back on.
func TestBarrierNoConsumerPods(t *testing.T) {
	for _, tt := range []struct {
		name        string
		objects     []client.Object
		wantBlocked bool
	}{
		{name: "no pods at all", wantBlocked: false},
		{
			name:        "a running pod holds the claim",
			objects:     []client.Object{consumerPod("exporter", corev1.PodRunning, testNames.ExportPVCName)},
			wantBlocked: true,
		},
		{
			name:        "a Succeeded pod still holds the claim",
			objects:     []client.Object{consumerPod("exporter", corev1.PodSucceeded, testNames.ExportPVCName)},
			wantBlocked: true,
		},
		{
			name:        "a Failed pod still holds the claim",
			objects:     []client.Object{consumerPod("exporter", corev1.PodFailed, testNames.ExportPVCName)},
			wantBlocked: true,
		},
		{
			// RWX and other shared volumes: any pod counts, not just the exporter's own.
			name:        "somebody else's pod holds the claim",
			objects:     []client.Object{consumerPod("a-stranger", corev1.PodRunning, testNames.ExportPVCName)},
			wantBlocked: true,
		},
		{
			name:        "a pod holding an unrelated claim",
			objects:     []client.Object{consumerPod("unrelated", corev1.PodRunning, "some-other-claim")},
			wantBlocked: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(recoverySchemeWithStorage(t)).WithObjects(tt.objects...).Build()
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			blocked, err := reconciler.barrierNoConsumerPods(context.Background(), testNames.ExportPVCName)
			require.NoError(t, err)
			if !tt.wantBlocked {
				assert.Nil(t, blocked)
				return
			}
			require.NotNil(t, blocked)
			assert.Equal(t, "B1", blocked.Name)
			assert.Contains(t, blocked.Message, blocked.Object.Name, "the blocking pod must be named in the message")
		})
	}
}

// The Pod informer this controller runs with holds only pods labelled as its own (cmd/main.go restricts
// it to app in (data-exporter, data-importer) in the controller namespace). Reading it would answer "no
// pod holds the claim" for a pod the controller merely is not watching, and the volume would then be
// taken away from under it — which is the single thing B1 is there to prevent.
func TestBarrierNoConsumerPodsReadsLivePodsNotTheFilteredCache(t *testing.T) {
	scheme := recoverySchemeWithStorage(t)
	stranger := consumerPod("a-stranger", corev1.PodRunning, testNames.ExportPVCName)
	// The cached client stands in for the filtered informer: it does not hold the stranger.
	cached := fake.NewClientBuilder().WithScheme(scheme).Build()
	live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stranger).Build()
	reconciler := createTestReconciler(cached, live, createTestConfig())

	blocked, err := reconciler.barrierNoConsumerPods(context.Background(), testNames.ExportPVCName)
	require.NoError(t, err)
	require.NotNil(t, blocked, "a pod outside the cache went unnoticed")
	assert.Equal(t, "a-stranger", blocked.Object.Name)
}

// TestBarrierNoVolumeAttachment covers B2, which asserts only that no attachment is active right now.
// Its absence is not read as evidence about the driver: safety for non-attachable volumes comes from B1.
func TestBarrierNoVolumeAttachment(t *testing.T) {
	for _, tt := range []struct {
		name        string
		objects     []client.Object
		wantBlocked bool
	}{
		{name: "no attachments", wantBlocked: false},
		{name: "attached", objects: []client.Object{volumeAttachment("va-1", "test-pv", true)}, wantBlocked: true},
		{
			// Not yet attached, but the object exists: the attach may still be in flight.
			name:        "present but not yet attached",
			objects:     []client.Object{volumeAttachment("va-1", "test-pv", false)},
			wantBlocked: true,
		},
		{
			name:        "attachment for another volume",
			objects:     []client.Object{volumeAttachment("va-1", "another-pv", true)},
			wantBlocked: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(recoverySchemeWithStorage(t)).WithObjects(tt.objects...).Build()
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			blocked, err := reconciler.barrierNoVolumeAttachment(context.Background(), "test-pv")
			require.NoError(t, err)
			assert.Equal(t, tt.wantBlocked, blocked != nil)
			if tt.wantBlocked {
				assert.Equal(t, "B2", blocked.Name)
			}
		})
	}
}

// TestBarrierExportPVCGone covers B3. It is evaluated against the UID the volume is bound to: a claim
// that merely reused the name is somebody else's object and does not keep the volume hostage.
func TestBarrierExportPVCGone(t *testing.T) {
	claim := func(uid types.UID, terminating bool) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: testNames.ExportPVCName, Namespace: testExportPVCNamespace, UID: uid},
		}
		if terminating {
			now := metav1.Now()
			pvc.DeletionTimestamp = &now
			pvc.Finalizers = []string{"kubernetes.io/pvc-protection"}
		}
		return pvc
	}

	for _, tt := range []struct {
		name        string
		objects     []client.Object
		wantBlocked bool
	}{
		{name: "claim is gone", wantBlocked: false},
		{name: "holder still exists", objects: []client.Object{claim(testExportPVCUID, false)}, wantBlocked: true},
		{
			name:        "holder is terminating",
			objects:     []client.Object{claim(testExportPVCUID, true)},
			wantBlocked: true,
		},
		{
			name:        "a foreign claim took the name",
			objects:     []client.Object{claim("a-recreated-claim-uid", false)},
			wantBlocked: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(recoverySchemeWithStorage(t)).WithObjects(tt.objects...).Build()
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			blocked, err := reconciler.barrierExportPVCGone(context.Background(), testNames.ExportPVCName, testExportPVCUID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantBlocked, blocked != nil)
			if tt.wantBlocked {
				assert.Equal(t, "B3", blocked.Name)
			}
		})
	}
}

// TestClassifyVolumeHolder pins who recovery is allowed to take the volume from. The answer comes from
// the binding as it stands, not from the record: the record says which claim we made, the binding says
// who owns the volume now, and only when those are the same object may it be taken away.
func TestClassifyVolumeHolder(t *testing.T) {
	exportClaim := types.NamespacedName{Namespace: testExportPVCNamespace, Name: testNames.ExportPVCName}
	sourceClaim := types.NamespacedName{Namespace: dataExportNamespace, Name: testUserPVCName}

	claimRef := func(ns, name string, uid types.UID) *corev1.ObjectReference {
		return &corev1.ObjectReference{Namespace: ns, Name: name, UID: uid}
	}

	for _, tt := range []struct {
		name              string
		claimRef          *corev1.ObjectReference
		recordedExportUID string
		want              volumeHolder
	}{
		{name: "unbound volume", want: holderNobody},
		{
			name:     "already back with its owner",
			claimRef: claimRef(sourceClaim.Namespace, sourceClaim.Name, testUserPVCUID),
			want:     holderSourceClaim,
		},
		{
			name:              "held by the claim this export made",
			claimRef:          claimRef(exportClaim.Namespace, exportClaim.Name, testExportPVCUID),
			recordedExportUID: string(testExportPVCUID),
			want:              holderExportClaim,
		},
		{
			// The dangerous one: same name, but a different object owns the volume now.
			name:              "held by a claim that reused our name",
			claimRef:          claimRef(exportClaim.Namespace, exportClaim.Name, "a-recreated-claim-uid"),
			recordedExportUID: string(testExportPVCUID),
			want:              holderStranger,
		},
		{
			// Pre-UID takeovers have nothing to compare against, so the claim named in the binding, in
			// our namespace, is the one we made.
			name:     "legacy takeover with nothing recorded",
			claimRef: claimRef(exportClaim.Namespace, exportClaim.Name, "some-old-uid"),
			want:     holderExportClaim,
		},
		{
			name:              "held by an unrelated claim",
			claimRef:          claimRef("kube-system", "someone-elses-claim", "another-uid"),
			recordedExportUID: string(testExportPVCUID),
			want:              holderStranger,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyVolumeHolder(tt.claimRef, exportClaim, sourceClaim, tt.recordedExportUID))
		})
	}
}

// TestBarrierBindingRestored covers B4, whose UID comparison is the whole point: a same-named but
// different claim would otherwise look like a completed restore.
func TestBarrierBindingRestored(t *testing.T) {
	sourcePVC := types.NamespacedName{Namespace: dataExportNamespace, Name: testUserPVCName}

	build := func(mutate func(pv *corev1.PersistentVolume, claim *corev1.PersistentVolumeClaim)) []client.Object {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pv", UID: testPVUID},
			Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{
				Namespace: sourcePVC.Namespace, Name: sourcePVC.Name, UID: testUserPVCUID,
			}},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
		}
		claim := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: sourcePVC.Name, Namespace: sourcePVC.Namespace, UID: testUserPVCUID},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "test-pv"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}
		if mutate != nil {
			mutate(pv, claim)
		}
		return []client.Object{pv, claim}
	}

	for _, tt := range []struct {
		name        string
		mutate      func(pv *corev1.PersistentVolume, claim *corev1.PersistentVolumeClaim)
		wantBlocked bool
	}{
		{name: "volume and claim are bound to each other", wantBlocked: false},
		{
			name: "volume still holds the export claim",
			mutate: func(pv *corev1.PersistentVolume, _ *corev1.PersistentVolumeClaim) {
				pv.Spec.ClaimRef.Name = testNames.ExportPVCName
			},
			wantBlocked: true,
		},
		{
			// Same name, different object: without comparing the live UID this would look restored.
			name: "the claim was recreated under the same name",
			mutate: func(_ *corev1.PersistentVolume, claim *corev1.PersistentVolumeClaim) {
				claim.UID = "a-recreated-claim-uid"
			},
			wantBlocked: true,
		},
		{
			name: "the storage layer has not confirmed the binding yet",
			mutate: func(_ *corev1.PersistentVolume, claim *corev1.PersistentVolumeClaim) {
				claim.Status.Phase = corev1.ClaimLost
			},
			wantBlocked: true,
		},
		{
			name: "the volume is not Bound yet",
			mutate: func(pv *corev1.PersistentVolume, _ *corev1.PersistentVolumeClaim) {
				pv.Status.Phase = corev1.VolumePending
			},
			wantBlocked: true,
		},
		{
			name:        "the volume is not bound at all",
			mutate:      func(pv *corev1.PersistentVolume, _ *corev1.PersistentVolumeClaim) { pv.Spec.ClaimRef = nil },
			wantBlocked: true,
		},
		{
			name:        "the claim does not name the volume back",
			mutate:      func(_ *corev1.PersistentVolume, claim *corev1.PersistentVolumeClaim) { claim.Spec.VolumeName = "" },
			wantBlocked: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(recoverySchemeWithStorage(t)).WithObjects(build(tt.mutate)...).Build()
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			blocked, err := reconciler.barrierBindingRestored(context.Background(), "test-pv", sourcePVC, string(testUserPVCUID))
			require.NoError(t, err)
			assert.Equal(t, tt.wantBlocked, blocked != nil)
			if tt.wantBlocked {
				assert.Equal(t, "B4", blocked.Name)
			}
		})
	}
}

// recoveringExport is a DataExport that has already detected the loss: the discriminator is set, the
// takeover identity is recorded, and the volume is still bound to the claim that no longer exists.
func recoveringExport() (*dev1alpha1.DataExport, *corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	dataExport, pv, _, userPVC := recoveryFixture()
	markRecovering(dataExport, pv, userPVC)
	return dataExport, pv, userPVC
}

// recoveringExportKeepingItsClaim is the same state one step earlier: the loss is recorded, but the export
// claim has not finished going away. Only a finalizer produces that with the fake client, which deletes
// instantly otherwise — and without it no test reaches the barrier that waits for the claim to be gone.
func recoveringExportKeepingItsClaim() (*dev1alpha1.DataExport, *corev1.PersistentVolume, *corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim) {
	dataExport, pv, exportPVC, userPVC := recoveryFixture()
	markRecovering(dataExport, pv, userPVC)
	exportPVC.Finalizers = append(exportPVC.Finalizers, "kubernetes.io/pvc-protection")
	return dataExport, pv, exportPVC, userPVC
}

// markRecovering puts the world into the state that follows a detected loss: the discriminator is set, the
// volume is still Bound to the claim taken over, and the user's claim has been Lost ever since the
// takeover repointed the volume away from it.
func markRecovering(dataExport *dev1alpha1.DataExport, pv *corev1.PersistentVolume, userPVC *corev1.PersistentVolumeClaim) {
	pv.Status.Phase = corev1.VolumeBound
	userPVC.Status.Phase = corev1.ClaimLost
	dataExport.Status.Phase = string(common.PhasePending)
	dataExport.Status.CleanupReason = string(common.CleanupReasonExportPVCPostRebindLost)
	dataExport.Status.Conditions = []metav1.Condition{{
		Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
		Reason: string(common.ReasonManagedResourceLost), LastTransitionTime: metav1.NewTime(time.Now()),
		Message: "export claim is gone",
	}}
}

// confirmBinding stands in for the PV controller, which is what actually completes the binding once the
// volume has been repointed at the user's claim.
func confirmBinding(t *testing.T, cl client.Client) {
	t.Helper()
	claim := &corev1.PersistentVolumeClaim{}
	require.NoError(t, cl.Get(context.Background(),
		types.NamespacedName{Namespace: dataExportNamespace, Name: testUserPVCName}, claim))
	claim.Status.Phase = corev1.ClaimBound
	require.NoError(t, cl.Status().Update(context.Background(), claim))
}

// TestRecovery_ReturnsTheVolumeAndSettlesFailed is the whole point of the P0 work: after the export claim
// is lost, the user gets the volume back and the object stops being stuck.
//
// It takes more than one pass, and that is the design: the controller repoints the volume, then waits for
// the storage layer to confirm the binding before undoing the protection it put on the volume. Nothing
// here remembers which pass it is on — each one re-reads the world and continues from what it finds.
func TestRecovery_ReturnsTheVolumeAndSettlesFailed(t *testing.T) {
	dataExport, pv, userPVC := recoveringExport()

	fakeClient := fake.NewClientBuilder().
		WithScheme(recoverySchemeWithStorage(t)).
		WithObjects(dataExport, pv, userPVC).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	_, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)

	awaiting := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, awaiting))
	assert.Equal(t, string(common.CleanupReasonExportPVCPostRebindLost), awaiting.Status.CleanupReason,
		"the recovery is still owed until the binding is confirmed")
	reboundPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, reboundPV))
	assert.Equal(t, corev1.PersistentVolumeReclaimRetain, reboundPV.Spec.PersistentVolumeReclaimPolicy,
		"the volume keeps its protection while the binding is unconfirmed")

	confirmBinding(t, fakeClient)

	_, err = reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	assert.Empty(t, got.Status.CleanupReason, "a finished recovery clears the discriminator")
	assert.Equal(t, string(common.PhaseFailed), got.Status.Phase, "and only then may the object settle")
	assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName,
		"the primitive restores the data plane; the parent's lifecycle stays with its own deletion path")

	gotPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, gotPV))
	require.NotNil(t, gotPV.Spec.ClaimRef)
	assert.Equal(t, testUserPVCName, gotPV.Spec.ClaimRef.Name)
	assert.Equal(t, testUserPVCUID, gotPV.Spec.ClaimRef.UID)
	assert.Equal(t, corev1.PersistentVolumeReclaimDelete, gotPV.Spec.PersistentVolumeReclaimPolicy,
		"the export-time Retain is undone only after the binding is confirmed")
	assertPVExportMetadataRemoved(t, gotPV)

	gotClaim := &corev1.PersistentVolumeClaim{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: dataExportNamespace, Name: testUserPVCName}, gotClaim))
	assert.NotContains(t, gotClaim.Finalizers, dev1alpha1.StorageManagerFinalizerName)
	assert.NotContains(t, gotClaim.Annotations, DataExportInProgressKey)
}

// TestRecovery_BarrierHoldsTheVolumeAndKeepsTheDiscriminator: while anything still holds the volume,
// nothing irreversible happens and the object keeps owing the recovery. A barrier is a state, not an
// error, and there is no timeout after which it is ignored.
func TestRecovery_BarrierHoldsTheVolumeAndKeepsTheDiscriminator(t *testing.T) {
	dataExport, pv, userPVC := recoveringExport()
	blocker := consumerPod("still-mounted", corev1.PodSucceeded, testNames.ExportPVCName)

	fakeClient := fake.NewClientBuilder().
		WithScheme(recoverySchemeWithStorage(t)).
		WithObjects(dataExport, pv, userPVC, blocker).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	result, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err, "a barrier is a state to wait on, not a failure")
	assert.NotZero(t, result.RequeueAfter)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	ready := assertReadyReason(t, got, common.ReasonCleanupBlocked)
	assert.Contains(t, ready.Message, "B1")
	assert.Contains(t, ready.Message, blocker.Name, "the blocking object must be named")
	assert.Equal(t, string(common.CleanupReasonExportPVCPostRebindLost), got.Status.CleanupReason,
		"the recovery is still owed")
	assert.NotEqual(t, string(common.PhaseFailed), got.Status.Phase)

	gotPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, gotPV))
	assert.Equal(t, testNames.ExportPVCName, gotPV.Spec.ClaimRef.Name, "the volume must not move while blocked")
	assert.Equal(t, corev1.PersistentVolumeReclaimRetain, gotPV.Spec.PersistentVolumeReclaimPolicy)

	gotClaim := &corev1.PersistentVolumeClaim{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: dataExportNamespace, Name: testUserPVCName}, gotClaim))
	assert.Contains(t, gotClaim.Finalizers, dev1alpha1.StorageManagerFinalizerName,
		"the source claim stays protected until its volume is actually back")
}

// TestRecovery_AttachedVolumeHoldsTheRecovery pins B2 where it is wired in rather than as a predicate on
// its own. The pod being gone is not the same as the volume being detached: in between, the node still has
// the device, and handing the volume back in that window gives it to an owner the old node may still write
// to. The barrier predicate has its own test; without this one, deleting the call would leave CI green.
func TestRecovery_AttachedVolumeHoldsTheRecovery(t *testing.T) {
	dataExport, pv, userPVC := recoveringExport()
	// Nothing holds the claim any more — B1 is satisfied, so only B2 can be what stops this.
	attachment := volumeAttachment("va-not-detached-yet", pv.Name, true)

	fakeClient := fake.NewClientBuilder().
		WithScheme(recoverySchemeWithStorage(t)).
		WithObjects(dataExport, pv, userPVC, attachment).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	result, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err, "an undetached volume is a state to wait on, not a failure")
	assert.NotZero(t, result.RequeueAfter)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	ready := assertReadyReason(t, got, common.ReasonCleanupBlocked)
	assert.Contains(t, ready.Message, "B2")
	assert.Contains(t, ready.Message, attachment.Name, "the blocking object must be named")
	assert.Equal(t, string(common.CleanupReasonExportPVCPostRebindLost), got.Status.CleanupReason,
		"the recovery is still owed while the volume is attached")

	gotPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, gotPV))
	require.NotNil(t, gotPV.Spec.ClaimRef)
	assert.Equal(t, testNames.ExportPVCName, gotPV.Spec.ClaimRef.Name,
		"the volume changed hands while a node still had it attached")
	assert.Equal(t, corev1.PersistentVolumeReclaimRetain, gotPV.Spec.PersistentVolumeReclaimPolicy)

	gotClaim := &corev1.PersistentVolumeClaim{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: dataExportNamespace, Name: testUserPVCName}, gotClaim))
	assert.NotEqual(t, corev1.ClaimBound, gotClaim.Status.Phase)
}

// TestRecovery_WaitsUntilTheExportClaimIsActuallyGone pins the other half of B3: deleting the claim is a
// request, not the event. While the claim is still there the volume is still legitimately bound to it, so
// repointing the binding then would take the volume away from an object that has not released it.
func TestRecovery_WaitsUntilTheExportClaimIsActuallyGone(t *testing.T) {
	dataExport, pv, exportPVC, userPVC := recoveringExportKeepingItsClaim()

	fakeClient := fake.NewClientBuilder().
		WithScheme(recoverySchemeWithStorage(t)).
		WithObjects(dataExport, pv, exportPVC, userPVC).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	result, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	ready := assertReadyReason(t, got, common.ReasonCleanupBlocked)
	assert.Contains(t, ready.Message, "B3")
	assert.Contains(t, ready.Message, testNames.ExportPVCName, "the blocking object must be named")

	// The claim was asked to go and is on its way out: that part is allowed to happen.
	stillThere := &corev1.PersistentVolumeClaim{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: testExportPVCNamespace, Name: testNames.ExportPVCName}, stillThere))
	assert.NotNil(t, stillThere.DeletionTimestamp, "the claim holding the volume was never asked to go")

	gotPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, gotPV))
	require.NotNil(t, gotPV.Spec.ClaimRef)
	assert.Equal(t, testNames.ExportPVCName, gotPV.Spec.ClaimRef.Name,
		"the volume was taken from a claim that still exists")
	assert.Equal(t, corev1.PersistentVolumeReclaimRetain, gotPV.Spec.PersistentVolumeReclaimPolicy)
}

// TestEnsureExportPVCGone_LeavesAClaimThatNamesAnotherExport is the teardown half of the provenance rule.
// The reasons a claim under the generated name counts as ours are inferred — from a binding, or from our
// own naming — and a snapshot export, which borrows no volume, has only the naming. The marker is the one
// piece of evidence a stranger cannot produce by accident, so a claim naming somebody else is not deleted
// even where every other reason says it is ours. A missing marker still passes: that is what a claim from
// the previous external-provisioner has.
func TestEnsureExportPVCGone_LeavesAClaimThatNamesAnotherExport(t *testing.T) {
	snapshotNames := common.NewNamesFromShort(dev1alpha1.KindSnapshotShort, "leaf1", dataExportNamespace, dataExportName)

	for _, tt := range []struct {
		name    string
		marker  string
		deleted bool
	}{
		{name: "our own marker", marker: string(testDataExportUID), deleted: true},
		{name: "no marker, as the previous executor left it", marker: "", deleted: true},
		{name: "the marker of another export", marker: "de-uid-of-somebody-else", deleted: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: snapshotNames.ExportPVCName, Namespace: testExportPVCNamespace,
				UID: testExportPVCUID, ResourceVersion: "1",
			}}
			if tt.marker != "" {
				claim.Annotations = map[string]string{dev1alpha1.AnnotationDataExportUIDKey: tt.marker}
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(recoverySchemeWithStorage(t)).
				WithObjects(claim).
				Build()
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			blocked, err := reconciler.ensureExportPVCGone(context.Background(), snapshotNames,
				takeoverRef{DataExportUID: testDataExportUID})
			require.NoError(t, err)
			assert.Nil(t, blocked, "a claim that is not ours is not a barrier either: there is nothing to wait for")

			err = fakeClient.Get(context.Background(),
				types.NamespacedName{Namespace: testExportPVCNamespace, Name: snapshotNames.ExportPVCName},
				&corev1.PersistentVolumeClaim{})
			if tt.deleted {
				assert.True(t, apierrors.IsNotFound(err), "the export's own claim was left behind")
				return
			}
			assert.NoError(t, err, "a claim belonging to another export was deleted")
		})
	}
}

// TestRecovery_BlockedWithoutDiscriminatorNeverEntersRecovery is the boundary between the two blocked
// states. CleanupBlocked from detection means the controller could not prove a safe target at all, so the
// barriers must not get to decide whether to proceed anyway: entry is keyed on the discriminator.
func TestRecovery_BlockedWithoutDiscriminatorNeverEntersRecovery(t *testing.T) {
	dataExport, pv, userPVC := recoveringExport()
	dataExport.Status.CleanupReason = ""
	dataExport.Status.Conditions = []metav1.Condition{{
		Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
		Reason: string(common.ReasonCleanupBlocked), LastTransitionTime: metav1.NewTime(time.Now()),
		Message: "the recorded volume was replaced",
	}}
	// The recorded volume is not the live one, which is exactly why detection refused to promise anything.
	pv.UID = "a-recreated-pv-uid"

	recorded := &mutationLog{}
	fakeClient := fake.NewClientBuilder().
		WithScheme(recoverySchemeWithStorage(t)).
		WithObjects(dataExport, pv, userPVC).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		WithInterceptorFuncs(recorded.interceptors()).
		Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	_, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)

	assert.Empty(t, recorded.writes, "no object may be touched on behalf of an unprovable recovery")

	gotPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, gotPV))
	assert.Equal(t, testNames.ExportPVCName, gotPV.Spec.ClaimRef.Name)

	gotClaim := &corev1.PersistentVolumeClaim{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: dataExportNamespace, Name: testUserPVCName}, gotClaim))
	assert.Contains(t, gotClaim.Finalizers, dev1alpha1.StorageManagerFinalizerName)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName)
	assert.Empty(t, got.Status.CleanupReason, "detection must not acquire a discriminator it could not justify")
}

// exportClaimNamed builds a claim under the generated export name with the given identity, standing in
// for whatever object holds that name at recovery time.
func exportClaimNamed(uid types.UID, pvName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNames.ExportPVCName, Namespace: testExportPVCNamespace, UID: uid, ResourceVersion: "1",
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// TestRecovery_WillNotTakeTheVolumeFromItsCurrentOwner is the case the recorded UID alone cannot see: a
// different claim now holds the volume. Deleting it is out of the question — we did not create it — and
// rebinding past it would take a bound volume away from its owner, which no record entitles us to do.
func TestRecovery_WillNotTakeTheVolumeFromItsCurrentOwner(t *testing.T) {
	const usurperUID = types.UID("a-claim-we-did-not-create")

	dataExport, pv, userPVC := recoveringExport()
	dataExport.Status.CleanupReason = string(common.CleanupReasonExportPVCIdentityMismatch)
	// The record still names the claim this export made; the volume has moved on to another one.
	pv.Spec.ClaimRef.UID = usurperUID
	usurper := exportClaimNamed(usurperUID, pv.Name)

	recorded := &mutationLog{}
	fakeClient := fake.NewClientBuilder().
		WithScheme(recoverySchemeWithStorage(t)).
		WithObjects(dataExport, pv, userPVC, usurper).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		WithInterceptorFuncs(recorded.interceptors()).
		Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	_, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)

	assert.Empty(t, recorded.writes, "nothing may be deleted or repointed while somebody else holds the volume")

	gotPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, gotPV))
	assert.Equal(t, usurperUID, gotPV.Spec.ClaimRef.UID, "the binding stays with its current owner")
	assert.Equal(t, corev1.PersistentVolumeReclaimRetain, gotPV.Spec.PersistentVolumeReclaimPolicy)

	gotClaim := &corev1.PersistentVolumeClaim{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: dataExportNamespace, Name: testUserPVCName}, gotClaim))
	assert.Contains(t, gotClaim.Finalizers, dev1alpha1.StorageManagerFinalizerName,
		"the source claim is not released while its volume has not come back")

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	assert.Equal(t, string(common.CleanupReasonExportPVCIdentityMismatch), got.Status.CleanupReason)
	ready := assertReadyReason(t, got, common.ReasonCleanupBlocked)
	assert.Contains(t, ready.Message, "B3")
	assert.Contains(t, ready.Message, testNames.ExportPVCName, "the blocking claim must be named")
}

// TestRecovery_IgnoresAClaimThatMerelyReusedTheName is the other side of the same distinction. Here the
// volume is still bound to the claim that no longer exists, so the namesake holds nothing: it is left
// alone and the volume goes back to its owner.
func TestRecovery_IgnoresAClaimThatMerelyReusedTheName(t *testing.T) {
	dataExport, pv, userPVC := recoveringExport()
	// pv.Spec.ClaimRef still carries the recorded export UID; the live namesake is a different object.
	namesake := exportClaimNamed("a-claim-that-took-the-name", "")

	fakeClient := fake.NewClientBuilder().
		WithScheme(recoverySchemeWithStorage(t)).
		WithObjects(dataExport, pv, userPVC, namesake).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	_, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)
	confirmBinding(t, fakeClient)
	_, err = reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)

	survivor := &corev1.PersistentVolumeClaim{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: testExportPVCNamespace, Name: testNames.ExportPVCName}, survivor))
	assert.Equal(t, types.UID("a-claim-that-took-the-name"), survivor.UID,
		"a claim we did not create must survive the recovery untouched")

	gotPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, gotPV))
	assert.Equal(t, testUserPVCUID, gotPV.Spec.ClaimRef.UID, "the volume goes back to its owner")

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	assert.Empty(t, got.Status.CleanupReason)
	assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)
}

// TestRecovery_TearsDownInfrastructureBeforeReleasingTheSourceClaim pins the order that matters on the
// teardown side: our finalizer on the user's claim is the last thing to go, because it is what keeps the
// claim alive while its volume is still in our hands.
func TestRecovery_TearsDownInfrastructureBeforeReleasingTheSourceClaim(t *testing.T) {
	dataExport, pv, userPVC := recoveringExport()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNames.DeployName, Namespace: testExportPVCNamespace,
			Labels: map[string]string{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue},
		},
	}
	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testNames.CASecretName, Namespace: testExportPVCNamespace},
	}

	recorded := &mutationLog{}
	fakeClient := fake.NewClientBuilder().
		WithScheme(recoverySchemeWithStorage(t)).
		WithObjects(dataExport, pv, userPVC, deploy, caSecret).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		WithInterceptorFuncs(recorded.interceptors()).
		Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	_, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)
	confirmBinding(t, fakeClient)
	_, err = reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)

	// The release shows up as the write that strips the annotation and finalizer from the user's claim.
	releasedAt := indexOfWrite(t, recorded.writes, "update "+testUserPVCName)
	assert.Less(t, indexOfWrite(t, recorded.writes, "delete "+testNames.CASecretName), releasedAt,
		"the CA secret goes before the user's claim is released")
	assert.Less(t, indexOfWrite(t, recorded.writes, "delete "+testNames.DeployName), releasedAt,
		"so does the exporter deployment")
}

// teardownEntry is one of the four ways the teardown is entered. They differ in what they do afterwards —
// release the object, settle it, or nothing at all, because there is no object left — and in nothing else.
type teardownEntry struct {
	name string
	// noParent marks the one entry that runs after the DataExport is already gone.
	noParent bool
	// status puts the object into the state that routes the reconcile into this entry.
	status func(dataExport *dev1alpha1.DataExport)
	// deleted asks for the object to be deleted before the reconcile.
	deleted bool
	// finished says whether this entry's own completion action has been taken.
	finished func(t *testing.T, cl client.Client, dataExport *dev1alpha1.DataExport) bool
}

func teardownEntries() []teardownEntry {
	releasedTheObject := func(t *testing.T, _ client.Client, dataExport *dev1alpha1.DataExport) bool {
		t.Helper()
		return dataExport == nil || !common.ContainsString(dataExport.Finalizers, dev1alpha1.StorageManagerFinalizerName)
	}

	return []teardownEntry{
		{
			name: "managed-resource recovery",
			status: func(dataExport *dev1alpha1.DataExport) {
				dataExport.Status.CleanupReason = string(common.CleanupReasonExportPVCPostRebindLost)
			},
			// Recovery settles the object rather than releasing it: the discriminator is what says the
			// controller still owes the volume.
			finished: func(_ *testing.T, _ client.Client, dataExport *dev1alpha1.DataExport) bool {
				return dataExport != nil && dataExport.Status.CleanupReason == ""
			},
		},
		{
			name:     "explicit deletion",
			deleted:  true,
			finished: releasedTheObject,
		},
		{
			name: "expiry",
			status: func(dataExport *dev1alpha1.DataExport) {
				dataExport.Status.ServerState = string(common.ServerStateIdleExpired)
			},
			finished: releasedTheObject,
		},
		{
			name:     "orphan sweep",
			noParent: true,
			// The sweep has no object to release or settle; giving the volume back is the whole of it.
			finished: func(t *testing.T, cl client.Client, _ *dev1alpha1.DataExport) bool {
				t.Helper()
				pv := &corev1.PersistentVolume{}
				require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, pv))
				return pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Name == testUserPVCName
			},
		},
	}
}

// TestTeardown_EveryEntryObeysTheSameContract is the §1.2 contract: the primitive restores the data plane
// and never touches the parent's finalizer; each caller performs its own lifecycle action, and only once
// the teardown reports completion. A barrier or an error must leave the parent exactly as it was — that
// finalizer is the only thing keeping the unfinished work reachable.
func TestTeardown_EveryEntryObeysTheSameContract(t *testing.T) {
	// world builds the objects behind a teardown that is held up by a pod still using the export claim.
	world := func(t *testing.T, entry teardownEntry, extra ...client.Object) (client.Client, *DataexportReconciler) {
		t.Helper()
		dataExport, pv, userPVC := recoveringExport()
		// Only the recovery entry owes a recovery; the others must reach the teardown through their own
		// branch, not through the discriminator.
		dataExport.Status.CleanupReason = ""
		if entry.status != nil {
			entry.status(dataExport)
		}

		objects := []client.Object{pv, userPVC}
		objects = append(objects, extra...)
		if !entry.noParent {
			objects = append(objects, dataExport)
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(recoverySchemeWithStorage(t)).
			WithObjects(objects...).
			WithStatusSubresource(&dev1alpha1.DataExport{}).
			Build()
		if entry.deleted {
			require.NoError(t, fakeClient.Delete(context.Background(), dataExport))
		}
		return fakeClient, createTestReconciler(fakeClient, fakeClient, createTestConfig())
	}

	// liveParent re-reads the object, or reports nil when there is none left to read.
	liveParent := func(t *testing.T, cl client.Client) *dev1alpha1.DataExport {
		t.Helper()
		got := &dev1alpha1.DataExport{}
		if err := cl.Get(context.Background(), deRequest.NamespacedName, got); err != nil {
			require.NoError(t, client.IgnoreNotFound(err))
			return nil
		}
		return got
	}

	for _, entry := range teardownEntries() {
		t.Run(entry.name+": a barrier stops the caller from finishing", func(t *testing.T) {
			cl, reconciler := world(t, entry, consumerPod("still-mounted", corev1.PodRunning, testNames.ExportPVCName))

			_, err := reconciler.Reconcile(context.Background(), deRequest)
			require.NoError(t, err, "a barrier is a state to wait on, not a failure")

			assert.False(t, entry.finished(t, cl, liveParent(t, cl)),
				"the caller must not take its lifecycle action while the teardown is unfinished")

			pv := &corev1.PersistentVolume{}
			require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, pv))
			assert.Equal(t, testNames.ExportPVCName, pv.Spec.ClaimRef.Name, "and the volume has not changed hands")
			assert.Contains(t, pv.Labels, dev1alpha1.LabelPVDataExporter)
		})

		t.Run(entry.name+": an error stops the caller from finishing", func(t *testing.T) {
			// A Deployment under our generated name that is not ours: the teardown refuses to touch it.
			foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Name: testNames.DeployName, Namespace: testExportPVCNamespace,
				Labels: map[string]string{dev1alpha1.LabelApplicationKey: "something-else"},
			}}
			cl, reconciler := world(t, entry, foreign)

			_, _ = reconciler.Reconcile(context.Background(), deRequest)

			assert.False(t, entry.finished(t, cl, liveParent(t, cl)),
				"a failed teardown must not be mistaken for a finished one")
		})

		t.Run(entry.name+": completion releases exactly what this path owns", func(t *testing.T) {
			cl, reconciler := world(t, entry)

			// The volume is repointed on the first pass; the storage layer confirms the binding, and the
			// second pass finishes.
			_, err := reconciler.Reconcile(context.Background(), deRequest)
			require.NoError(t, err)
			confirmBinding(t, cl)
			_, err = reconciler.Reconcile(context.Background(), deRequest)
			require.NoError(t, err)

			assert.True(t, entry.finished(t, cl, liveParent(t, cl)), "the caller may finish once the volume is back")

			pv := &corev1.PersistentVolume{}
			require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, pv))
			require.NotNil(t, pv.Spec.ClaimRef)
			assert.Equal(t, testUserPVCName, pv.Spec.ClaimRef.Name)
			assertPVExportMetadataRemoved(t, pv)
		})
	}
}

func indexOfWrite(t *testing.T, writes []string, want string) int {
	t.Helper()
	for i, write := range writes {
		if write == want {
			return i
		}
	}
	t.Fatalf("expected write %q, got %v", want, writes)
	return -1
}
