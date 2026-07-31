package dataexport

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// testExportPVCNamespace is the controller namespace the export claim lives in (see createTestConfig).
const testExportPVCNamespace = "test-namespace"

// mutationLog records every write the reconcile attempts, so a detection pass can be asserted to change
// nothing in the cluster. Status writes on the DataExport itself are the reconcile's own output and are
// counted separately.
type mutationLog struct {
	writes       []string
	statusWrites []dev1alpha1.DataExportImportStatus
}

func (m *mutationLog) interceptors() interceptor.Funcs {
	return interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			m.writes = append(m.writes, "delete "+obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			m.writes = append(m.writes, "patch "+obj.GetName())
			return cl.Patch(ctx, obj, patch, opts...)
		},
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			m.writes = append(m.writes, "create "+obj.GetName())
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ours := obj.(*dev1alpha1.DataExport); !ours {
				m.writes = append(m.writes, "update "+obj.GetName())
			}
			return cl.Update(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if de, ours := obj.(*dev1alpha1.DataExport); ours {
				m.statusWrites = append(m.statusWrites, *de.Status.DeepCopy())
			} else {
				m.writes = append(m.writes, "status "+obj.GetName())
			}
			return cl.Status().Update(ctx, obj, opts...)
		},
	}
}

// recoveryFixture builds a fully provisioned PVC export: the PV has been taken over and is bound to the
// export claim, and the takeover identity is recorded both on the PV and in the export status.
func recoveryFixture() (*dev1alpha1.DataExport, *corev1.PersistentVolume, *corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim) {
	dataExport := &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{
			Name: dataExportName, Namespace: dataExportNamespace, UID: testDataExportUID,
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec: dev1alpha1.DataExportSpec{
			TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: "PersistentVolumeClaim", Name: testUserPVCName},
			Ttl:       "1h",
		},
		Status: dev1alpha1.DataExportImportStatus{
			Phase: string(common.PhaseReady),
			Conditions: []metav1.Condition{{
				Type: string(common.ConditionReady), Status: metav1.ConditionTrue,
				Reason: string(common.ReasonServerReady), LastTransitionTime: metav1.NewTime(time.Now()),
			}},
			Recovery: &dev1alpha1.RecoveryStatus{
				SourcePVCUID: string(testUserPVCUID),
				ExportPVCUID: string(testExportPVCUID),
				PVName:       "test-pv",
				PVUID:        string(testPVUID),
			},
		},
	}

	annotations := withUIDAnnotations(testDataExportUID, testUserPVCUID)
	annotations[dev1alpha1.AnnotationUserPVCNamespaceKey] = dataExportNamespace
	annotations[dev1alpha1.AnnotationUserPVCNameKey] = testUserPVCName
	annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] = dataExportNamespace
	annotations[dev1alpha1.AnnotationStorageManagerNameKey] = dataExportName

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pv", UID: testPVUID, ResourceVersion: "1",
			Annotations: annotations,
			Labels:      map[string]string{dev1alpha1.LabelPVDataExporter: "true"},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef: &corev1.ObjectReference{
				Namespace: testExportPVCNamespace, Name: testNames.ExportPVCName, UID: testExportPVCUID,
			},
		},
	}

	exportPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNames.ExportPVCName, Namespace: testExportPVCNamespace, UID: testExportPVCUID, ResourceVersion: "1",
			Annotations: map[string]string{dev1alpha1.AnnotationDataExportUIDKey: string(testDataExportUID)},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	// The user's claim survives the takeover unbound: the PV was repointed away from it.
	userPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testUserPVCName, Namespace: dataExportNamespace, UID: testUserPVCUID, ResourceVersion: "1",
			Annotations: map[string]string{DataExportInProgressKey: "true"},
			Finalizers:  []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name},
	}

	return dataExport, pv, exportPVC, userPVC
}

// runRecoveryReconcile drives one reconcile and returns the persisted object. The reconcile error is
// returned rather than asserted: the detection paths must be error-free, but the ordinary provisioning
// path in these fixtures legitimately fails further down (no exporter image ConfigMap), and that says
// nothing about detection.
func runRecoveryReconcile(t *testing.T, objs ...client.Object) (*dev1alpha1.DataExport, *mutationLog, error) {
	t.Helper()

	recorded := &mutationLog{}
	fakeClient := fake.NewClientBuilder().
		WithScheme(setupTestScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		WithInterceptorFuncs(recorded.interceptors()).
		Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	_, reconcileErr := reconciler.Reconcile(context.Background(), deRequest)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: dataExportNamespace, Name: dataExportName}, got))
	return got, recorded, reconcileErr
}

func assertReadyReason(t *testing.T, de *dev1alpha1.DataExport, reason common.ConditionReason) *metav1.Condition {
	t.Helper()
	ready := meta.FindStatusCondition(de.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, string(reason), ready.Reason)
	return ready
}

// TestClassifyTakeoverState walks the whole matrix over the three witnesses, which is the point of
// keeping the classifier free of API reads: every combination is expressible here as plain values.
func TestClassifyTakeoverState(t *testing.T) {
	const recordedExportUID = testExportPVCUID

	for _, tt := range []struct {
		name   string
		mutate func(de *dev1alpha1.DataExport, pvc **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume)
		want   takeoverStateKind
	}{
		{
			name: "record, claim and volume all agree",
			want: takeoverHealthy,
		},
		{
			name: "claim gone while the volume still holds it",
			mutate: func(_ *dev1alpha1.DataExport, pvc **corev1.PersistentVolumeClaim, _ **corev1.PersistentVolume) {
				*pvc = nil
			},
			want: takeoverExportPVCLost,
		},
		{
			name: "a claim with our name but a foreign UID",
			mutate: func(_ *dev1alpha1.DataExport, pvc **corev1.PersistentVolumeClaim, _ **corev1.PersistentVolume) {
				(*pvc).UID = "a-recreated-claim-uid"
			},
			want: takeoverIdentityMismatch,
		},
		{
			name: "the recorded claim exists but the volume moved to another one",
			mutate: func(_ *dev1alpha1.DataExport, _ **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				(*pv).Spec.ClaimRef.UID = "a-claim-that-is-not-ours"
			},
			want: takeoverIdentityMismatch,
		},
		{
			name: "claim gone and the volume was not holding it either",
			mutate: func(_ *dev1alpha1.DataExport, pvc **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				*pvc = nil
				(*pv).Spec.ClaimRef.UID = "a-claim-that-is-not-ours"
			},
			want: takeoverIdentityMismatch,
		},
		{
			name: "the volume is bound to something outside our namespace",
			mutate: func(_ *dev1alpha1.DataExport, _ **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				(*pv).Spec.ClaimRef.Namespace = "someone-else"
			},
			want: takeoverIdentityMismatch,
		},
		{
			name: "the recorded volume was replaced under its name",
			mutate: func(_ *dev1alpha1.DataExport, _ **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				(*pv).UID = "a-recreated-pv-uid"
			},
			want: takeoverPVUnverified,
		},
		{
			name: "the recorded volume is gone",
			mutate: func(_ *dev1alpha1.DataExport, _ **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				*pv = nil
			},
			want: takeoverPVUnverified,
		},
		{
			name: "nothing taken over yet",
			mutate: func(de *dev1alpha1.DataExport, _ **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				de.Status.Recovery = nil
				*pv = nil
			},
			want: takeoverHealthy,
		},
		{
			name: "legacy takeover, claim gone",
			mutate: func(de *dev1alpha1.DataExport, pvc **corev1.PersistentVolumeClaim, _ **corev1.PersistentVolume) {
				de.Status.Recovery = nil
				*pvc = nil
			},
			want: takeoverLegacyLossUnprovable,
		},
		{
			// Before the rebind the claim is the only object in play, so its marker is the only thing
			// that can tell an export's own claim from a namesake.
			name: "nothing taken over yet and the claim carries no marker",
			mutate: func(de *dev1alpha1.DataExport, pvc **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				de.Status.Recovery = nil
				*pv = nil
				(*pvc).Annotations = nil
			},
			want: takeoverExportPVCUnproven,
		},
		{
			name: "nothing taken over yet and the claim belongs to another export",
			mutate: func(de *dev1alpha1.DataExport, pvc **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				de.Status.Recovery = nil
				*pv = nil
				(*pvc).Annotations[dev1alpha1.AnnotationDataExportUIDKey] = "uid-of-another-data-export"
			},
			want: takeoverExportPVCUnproven,
		},
		{
			// A takeover that predates identity entirely: no record, no marker. The volume is the only
			// witness left, and it says this claim is the one it was handed to.
			name: "legacy takeover whose unmarked claim still holds the volume",
			mutate: func(de *dev1alpha1.DataExport, pvc **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				de.Status.Recovery = nil
				(*pvc).Annotations = nil
				delete((*pv).Annotations, dev1alpha1.AnnotationDataExportUIDKey)
				delete((*pv).Annotations, dev1alpha1.AnnotationUserPVCUIDKey)
			},
			want: takeoverHealthy,
		},
		{
			// Without a record the volume is the only witness left, and it still contradicts the claim.
			name: "legacy takeover whose claim no longer holds the volume",
			mutate: func(de *dev1alpha1.DataExport, _ **corev1.PersistentVolumeClaim, pv **corev1.PersistentVolume) {
				de.Status.Recovery = nil
				(*pv).Spec.ClaimRef.UID = "a-claim-that-is-not-ours"
			},
			want: takeoverIdentityMismatch,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataExport, pv, exportPVC, _ := recoveryFixture()
			require.Equal(t, string(recordedExportUID), dataExport.Status.Recovery.ExportPVCUID)
			if tt.mutate != nil {
				tt.mutate(dataExport, &exportPVC, &pv)
			}

			got := classifyTakeoverState(dataExport, testNames, exportPVC, pv, testExportPVCNamespace)
			assert.Equal(t, tt.want, got.kind, "message was: %s", got.message)
			if tt.want != takeoverHealthy {
				assert.NotEmpty(t, got.message, "a non-healthy state must explain itself on the object")
			}
		})
	}
}

// TestSnapshotClaimIsExemptUntilTheExecutorRolloutCompletes pins the one exemption in the provenance
// rule, because a comment alone does not survive a reader who checks its premise. The premise has already
// changed once: the external-provisioner now copies pvcTemplate.metadata onto the claim it creates, so
// snapshot claims made by the current executor DO carry the marker, and the exemption can look obsolete.
//
// It is not. Claims created by the previous executor carry nothing, and enforcing before that image is
// everywhere strands every in-flight snapshot export on CleanupBlocked with no way back. Removing the
// exemption is one half of a rollout-gated change whose other half is teardown (which today deletes a
// snapshot claim as "ours by construction"); this test is what fails if only the first half is done.
func TestSnapshotClaimIsExemptUntilTheExecutorRolloutCompletes(t *testing.T) {
	dataExport := &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{Name: dataExportName, Namespace: dataExportNamespace, UID: testDataExportUID},
		Spec: dev1alpha1.DataExportSpec{
			TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindVolumeSnapshot, Name: "leaf1"},
		},
	}
	snapshotNames := common.NewNamesFromShort(dev1alpha1.KindSnapshotShort, "leaf1", dataExportNamespace, dataExportName)

	// A claim from the previous executor: right name, no marker, and no volume to vouch for it.
	unmarked := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotNames.ExportPVCName, Namespace: testExportPVCNamespace, UID: testExportPVCUID,
		},
	}

	got := classifyTakeoverState(dataExport, snapshotNames, unmarked, nil, testExportPVCNamespace)
	assert.Equal(t, takeoverHealthy, got.kind,
		"snapshot exports must stay usable without a marker until the updated external-provisioner is rolled out everywhere; enable this together with teardown, not on its own")

	// The exemption is scoped to the path that does not create its own claim. The same unmarked claim
	// under an export that creates its claim itself proves nothing and may not be used.
	pvcNames := common.NewNamesFromShort(dev1alpha1.KindPVCShort, testUserPVCName, dataExportNamespace, dataExportName)
	unmarked.Name = pvcNames.ExportPVCName
	got = classifyTakeoverState(dataExport, pvcNames, unmarked, nil, testExportPVCNamespace)
	assert.Equal(t, takeoverExportPVCUnproven, got.kind, "message was: %s", got.message)
}

// TestReconcile_DetectsPostRebindExportPVCLoss is the P0 leak itself: someone deletes the export claim of
// a serving export. The volume is already bound to that dead claim's UID, so nothing can recreate it and
// the user's PVC stays hostage. The reconcile must say so and claim the recovery, without touching a
// single object on the pass that discovers it.
func TestReconcile_DetectsPostRebindExportPVCLoss(t *testing.T) {
	dataExport, pv, _, userPVC := recoveryFixture()

	got, recorded, err := runRecoveryReconcile(t, dataExport, pv, userPVC)
	require.NoError(t, err, "detection must not surface as a reconcile error")

	assertReadyReason(t, got, common.ReasonManagedResourceLost)
	assert.Equal(t, string(common.CleanupReasonExportPVCPostRebindLost), got.Status.CleanupReason)
	assert.Equal(t, string(common.PhasePending), got.Status.Phase, "an object owing recovery must not be terminal")
	assert.Empty(t, recorded.writes, "detection must not mutate the cluster")
}

// TestReconcile_DetectionWritesTheDiscriminatorAtomically pins the invariant the phase gate rests on: an
// empty cleanupReason next to a managed-resource failure reason means "recovery already finished". If the
// condition were ever persisted before the discriminator, that intermediate object would be read as a
// finished recovery and settle as Failed with the volume still taken over.
func TestReconcile_DetectionWritesTheDiscriminatorAtomically(t *testing.T) {
	dataExport, pv, _, userPVC := recoveryFixture()

	_, recorded, err := runRecoveryReconcile(t, dataExport, pv, userPVC)
	require.NoError(t, err)

	require.NotEmpty(t, recorded.statusWrites)
	for i, status := range recorded.statusWrites {
		ready := meta.FindStatusCondition(status.Conditions, string(common.ConditionReady))
		require.NotNil(t, ready, "status write %d", i)
		if ready.Status == metav1.ConditionFalse && common.IsManagedResourceFailureReason(common.ConditionReason(ready.Reason)) {
			assert.NotEmpty(t, status.CleanupReason,
				"status write %d announced the failure without the discriminator that owes the recovery", i)
		}
	}
}

// TestReconcile_PreRebindExportPVCLossIsOrdinaryDrift is the boundary of the previous test: before the
// volume changes hands, a deleted export claim is just a missing resource. Provisioning recreates it and
// no recovery is owed.
func TestReconcile_PreRebindExportPVCLossIsOrdinaryDrift(t *testing.T) {
	dataExport, pv, _, userPVC := recoveryFixture()
	dataExport.Status.Phase = string(common.PhasePending)
	dataExport.Status.Conditions = []metav1.Condition{{
		Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
		Reason: string(common.ReasonPending), LastTransitionTime: metav1.NewTime(time.Now()),
	}}
	dataExport.Status.Recovery = nil
	// The takeover never happened: the PV is still the user's.
	pv.Annotations = makeFullAnnotations()
	pv.Labels = nil
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: userPVC.Namespace, Name: userPVC.Name, UID: userPVC.UID,
	}
	userPVC.Annotations = nil
	userPVC.Finalizers = nil
	userPVC.Status.Phase = corev1.ClaimBound

	got, _, _ := runRecoveryReconcile(t, dataExport, pv, userPVC)

	assert.Empty(t, got.Status.CleanupReason, "a claim that can still be recreated owes no recovery")
	ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready)
	assert.False(t, common.IsManagedResourceFailureReason(common.ConditionReason(ready.Reason)),
		"ordinary provisioning drift must not be reported as a lost managed resource")
}

// TestReconcile_DetectsExportPVCIdentityMismatch covers the second P0 case: the claim was deleted and
// something recreated it under the same name. It looks healthy, but the volume is still bound to the
// dead claim's UID, so the export is serving nothing and the recovery is owed just the same.
func TestReconcile_DetectsExportPVCIdentityMismatch(t *testing.T) {
	dataExport, pv, exportPVC, userPVC := recoveryFixture()
	exportPVC.UID = "a-recreated-export-claim-uid"

	got, recorded, err := runRecoveryReconcile(t, dataExport, pv, exportPVC, userPVC)
	require.NoError(t, err, "detection must not surface as a reconcile error")

	assertReadyReason(t, got, common.ReasonManagedResourceIdentityMismatch)
	assert.Equal(t, string(common.CleanupReasonExportPVCIdentityMismatch), got.Status.CleanupReason)
	assert.Empty(t, recorded.writes, "the impostor claim must not be deleted on the detection pass")
}

// TestReconcile_LegacyPostRebindLossBlocksInsteadOfClaimingRecovery is rule 3 of the legacy contract: the
// loss is real, but without a recorded identity the controller cannot prove which claim the volume should
// go back to. It reports the blockage and deliberately does not take the recovery on, because that would
// promise a restore it cannot perform safely.
func TestReconcile_LegacyPostRebindLossBlocksInsteadOfClaimingRecovery(t *testing.T) {
	dataExport, pv, _, userPVC := recoveryFixture()
	dataExport.Status.Recovery = nil
	delete(pv.Annotations, dev1alpha1.AnnotationDataExportUIDKey)
	delete(pv.Annotations, dev1alpha1.AnnotationUserPVCUIDKey)

	got, recorded, err := runRecoveryReconcile(t, dataExport, pv, userPVC)
	require.NoError(t, err, "a blocked cleanup is a reported state, not a reconcile error")

	ready := assertReadyReason(t, got, common.ReasonCleanupBlocked)
	assert.Contains(t, ready.Message, "identity")
	assert.Empty(t, got.Status.CleanupReason,
		"an unprovable restore must not be announced as an owed recovery")
	assert.NotEqual(t, string(common.PhaseFailed), got.Status.Phase, "CleanupBlocked is not an outcome")
	assert.Empty(t, recorded.writes)
}

// TestReconcile_DetectsClaimThatNoLongerHoldsTheVolume closes the third edge of the identity triangle.
// The live claim is the very one the takeover recorded, so both claim-side witnesses agree — but the
// volume itself is held by a different UID, so the export serves nothing. Comparing the record with the
// claim alone would call this healthy.
func TestReconcile_DetectsClaimThatNoLongerHoldsTheVolume(t *testing.T) {
	dataExport, pv, exportPVC, userPVC := recoveryFixture()
	pv.Spec.ClaimRef.UID = "a-claim-that-is-not-ours"

	got, recorded, err := runRecoveryReconcile(t, dataExport, pv, exportPVC, userPVC)
	require.NoError(t, err)

	assertReadyReason(t, got, common.ReasonManagedResourceIdentityMismatch)
	assert.Equal(t, string(common.CleanupReasonExportPVCIdentityMismatch), got.Status.CleanupReason)
	assert.Empty(t, recorded.writes)
}

// TestReconcile_RecordedPVReplacedIsNotAnOwedRecovery guards the object the whole recovery reasons about.
// A PV recreated under the recorded name is somebody else's volume; promising a recovery here would aim
// the next step's restore at it.
func TestReconcile_RecordedPVReplacedIsNotAnOwedRecovery(t *testing.T) {
	dataExport, pv, _, userPVC := recoveryFixture()
	pv.UID = "a-recreated-pv-uid"

	got, recorded, err := runRecoveryReconcile(t, dataExport, pv, userPVC)
	require.NoError(t, err)

	ready := assertReadyReason(t, got, common.ReasonCleanupBlocked)
	assert.Contains(t, ready.Message, pv.Name)
	assert.Empty(t, got.Status.CleanupReason, "an unverified volume must not be handed to recovery")
	assert.Empty(t, recorded.writes)
}

// TestReconcile_RecordedPVGoneIsBlockedNotHealthy: the recorded volume vanished entirely. Nothing can be
// restored, and treating the absence as "nothing was ever taken over" would let provisioning start a
// second takeover.
func TestReconcile_RecordedPVGoneIsBlockedNotHealthy(t *testing.T) {
	dataExport, _, exportPVC, userPVC := recoveryFixture()

	got, recorded, err := runRecoveryReconcile(t, dataExport, exportPVC, userPVC)
	require.NoError(t, err)

	assertReadyReason(t, got, common.ReasonCleanupBlocked)
	assert.Empty(t, got.Status.CleanupReason)
	assert.Empty(t, recorded.writes)
}

// TestReconcile_LostClaimWhoseVolumeMovedOnIsMismatchNotLoss keeps the two failures distinct: the claim is
// indeed gone, but the volume is not even held by the claim we recorded, so "lost" would understate a
// stronger contradiction and send the recovery after the wrong object.
func TestReconcile_LostClaimWhoseVolumeMovedOnIsMismatchNotLoss(t *testing.T) {
	dataExport, pv, _, userPVC := recoveryFixture()
	pv.Spec.ClaimRef.UID = "some-other-claim-uid"

	got, recorded, err := runRecoveryReconcile(t, dataExport, pv, userPVC)
	require.NoError(t, err)

	assertReadyReason(t, got, common.ReasonManagedResourceIdentityMismatch)
	assert.Equal(t, string(common.CleanupReasonExportPVCIdentityMismatch), got.Status.CleanupReason)
	assert.Empty(t, recorded.writes)
}

// TestReconcile_ForeignPVOwnerStaysPendingWithoutRecovery keeps mismatch B out of the recovery machinery:
// the PV belongs to another DataExport, so this object never took the volume over and has nothing to
// restore. It must wait, not claim a recovery and not touch the other export's resources.
func TestReconcile_ForeignPVOwnerStaysPendingWithoutRecovery(t *testing.T) {
	dataExport, pv, _, userPVC := recoveryFixture()
	dataExport.Status.Phase = string(common.PhasePending)
	dataExport.Status.Conditions = []metav1.Condition{{
		Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
		Reason: string(common.ReasonPending), LastTransitionTime: metav1.NewTime(time.Now()),
	}}
	dataExport.Status.Recovery = nil
	pv.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] = "another-data-export"
	pv.Annotations[dev1alpha1.AnnotationDataExportUIDKey] = "another-data-export-uid"
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: userPVC.Namespace, Name: userPVC.Name, UID: userPVC.UID,
	}

	got, recorded, _ := runRecoveryReconcile(t, dataExport, pv, userPVC)

	assert.Empty(t, got.Status.CleanupReason)
	assert.Equal(t, string(common.PhasePending), got.Status.Phase)
	assert.Empty(t, recorded.writes, "another export's volume must be left alone")
}
