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

package dataexport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

const (
	trackingPVName        = "pvc-snapshot-restore"
	trackingExportPVCName = "export-pvc-snap"
)

// snapshotNames mirrors the real flow, which resolves the short kind from the GroupKind targetRef and
// builds the names from it: every snapshot leaf, whatever its kind, exports under KindSnapshotShort.
func snapshotNames() common.Names {
	return common.NewNamesFromShort(dev1alpha1.KindSnapshotShort, "test-vs", dataExportNamespace, dataExportName)
}

// exportPVCBoundTo builds the export PVC as it looks once the provisioner has bound it.
func exportPVCBoundTo(t *testing.T, name, pvName string) *corev1.PersistentVolumeClaim {
	t.Helper()
	mode := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: createTestConfig().ControllerNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pvName, VolumeMode: &mode},
	}
}

func restorePV(name string, policy corev1.PersistentVolumeReclaimPolicy, claimRef *corev1.ObjectReference) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: policy,
			ClaimRef:                      claimRef,
		},
	}
}

// TestEnsureExportPVReady_SnapshotPVIsTracked is the guard for the defect: a snapshot export's PV was
// skipped outright, so it carried no tracking label. That label is what removeOrphanResources lists on, and
// the deletion of the orphaned deployment and export PVC happens inside that loop — an unlabelled PV means
// the loop body never runs and the cleanup reports success having deleted nothing, leaving an exporter pod
// serving a DataExport that no longer exists.
func TestEnsureExportPVReady_SnapshotPVIsTracked(t *testing.T) {
	scheme := setupTestScheme()
	cfg := createTestConfig()
	names := snapshotNames()

	claimRef := &corev1.ObjectReference{Namespace: cfg.ControllerNamespace, Name: trackingExportPVCName}
	pv := restorePV(trackingPVName, corev1.PersistentVolumeReclaimDelete, claimRef)
	exportPVC := exportPVCBoundTo(t, trackingExportPVCName, trackingPVName)
	dataExport := createDataExport(dev1alpha1.DataExportSpec{TTL: "10m"})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv, exportPVC).Build()
	r := createTestReconciler(c, c, cfg)

	// pv is passed as nil, as it is for a snapshot target: nothing fetched it before the deployment.
	require.NoError(t, r.ensureExportPVReady(context.Background(), nil, exportPVC, names, dataExport, ""))

	updated := &corev1.PersistentVolume{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: trackingPVName}, updated))

	assert.Equal(t, "true", updated.Labels[dev1alpha1.LabelPVDataExporter],
		"without this label removeOrphanResources cannot find the PV, and its loop is where the orphaned deployment and PVC are deleted")
	assert.Equal(t, dataExportNamespace, updated.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey])
	assert.Equal(t, dataExportName, updated.Annotations[dev1alpha1.AnnotationStorageManagerNameKey])
	assert.Equal(t, names.TargetKindShort, updated.Annotations[dev1alpha1.AnnotationPVTargetKindShortKey])
	assert.Equal(t, names.HashSuffix, updated.Annotations[dev1alpha1.AnnotationPVHashSuffixKey])

	// The user PVC name is empty for a snapshot export and must stay empty: recoverOrphanedUserPVC keys
	// the "nothing to reattach" branch on it.
	assert.Empty(t, updated.Annotations[dev1alpha1.AnnotationUserPVCNameKey])

	// The reclaim policy is recorded but not changed: the volume is a transient restore of a snapshot that
	// still exists, so Retain would leave a volume nothing deletes.
	assert.Equal(t, string(corev1.PersistentVolumeReclaimDelete),
		updated.Annotations[dev1alpha1.AnnotationOriginalReclaimPolicyKey])
	assert.Equal(t, corev1.PersistentVolumeReclaimDelete, updated.Spec.PersistentVolumeReclaimPolicy,
		"a snapshot restore volume must keep the policy its storage class gave it")

	// The claim binding is the provisioner's and must not be rewritten.
	require.NotNil(t, updated.Spec.ClaimRef)
	assert.Equal(t, trackingExportPVCName, updated.Spec.ClaimRef.Name)

	// What was written must be enough for orphan cleanup to accept the PV.
	info, err := parsePVRecoveryInfo(updated, dataExportNamespace, dataExportName)
	require.NoError(t, err, "the tracking metadata must satisfy the orphan-cleanup parser")
	assert.Empty(t, info.UserPVCName)
	assert.Equal(t, names.HashSuffix, info.HashSuffix)
}

// TestEnsureExportPVReady_SnapshotWaitsForBinding covers the ordering half: under a
// WaitForFirstConsumer storage class the provisioner creates the PV only after a pod references the claim,
// so an export PVC with no volumeName yet is a state to wait in, not an error to fail on.
func TestEnsureExportPVReady_SnapshotWaitsForBinding(t *testing.T) {
	scheme := setupTestScheme()
	cfg := createTestConfig()
	mode := corev1.PersistentVolumeFilesystem

	unbound := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: trackingExportPVCName, Namespace: cfg.ControllerNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &mode},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(unbound).Build()
	r := createTestReconciler(c, c, cfg)

	err := r.ensureExportPVReady(context.Background(), nil, unbound, snapshotNames(),
		createDataExport(dev1alpha1.DataExportSpec{TTL: "10m"}), "")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetNotReady,
		"an unbound export PVC must requeue as not-ready, not fail the reconcile")
}

// TestEnsureExportPVReady_SnapshotRereadsTheClaim pins why the claim is re-read: the caller's copy predates
// the binding, so trusting its empty volumeName would report not-ready forever.
func TestEnsureExportPVReady_SnapshotRereadsTheClaim(t *testing.T) {
	scheme := setupTestScheme()
	cfg := createTestConfig()
	names := snapshotNames()

	claimRef := &corev1.ObjectReference{Namespace: cfg.ControllerNamespace, Name: trackingExportPVCName}
	pv := restorePV(trackingPVName, corev1.PersistentVolumeReclaimDelete, claimRef)

	// In the API the claim is bound...
	bound := exportPVCBoundTo(t, trackingExportPVCName, trackingPVName)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv, bound).Build()
	r := createTestReconciler(c, c, cfg)

	// ...while the copy the caller holds is the pre-binding one.
	mode := corev1.PersistentVolumeFilesystem
	stale := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: trackingExportPVCName, Namespace: cfg.ControllerNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &mode},
	}

	require.NoError(t, r.ensureExportPVReady(context.Background(), nil, stale, names,
		createDataExport(dev1alpha1.DataExportSpec{TTL: "10m"}), ""))

	updated := &corev1.PersistentVolume{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: trackingPVName}, updated))
	assert.Equal(t, "true", updated.Labels[dev1alpha1.LabelPVDataExporter])
}

// TestEnsureExportPVReady_SnapshotIsIdempotent checks the restart path: a second pass over an
// already-tracked PV must not patch it again, and in particular must not re-record the reclaim policy,
// which by then is whatever the first pass left.
func TestEnsureExportPVReady_SnapshotIsIdempotent(t *testing.T) {
	scheme := setupTestScheme()
	cfg := createTestConfig()
	names := snapshotNames()
	dataExport := createDataExport(dev1alpha1.DataExportSpec{TTL: "10m"})

	claimRef := &corev1.ObjectReference{Namespace: cfg.ControllerNamespace, Name: trackingExportPVCName}
	pv := restorePV(trackingPVName, corev1.PersistentVolumeReclaimDelete, claimRef)
	exportPVC := exportPVCBoundTo(t, trackingExportPVCName, trackingPVName)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv, exportPVC).Build()
	r := createTestReconciler(c, c, cfg)

	require.NoError(t, r.ensureExportPVReady(context.Background(), nil, exportPVC, names, dataExport, ""))

	first := &corev1.PersistentVolume{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: trackingPVName}, first))

	require.NoError(t, r.ensureExportPVReady(context.Background(), nil, exportPVC, names, dataExport, ""))

	second := &corev1.PersistentVolume{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: trackingPVName}, second))

	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"an already-tracked PV must not be patched a second time")
}

// TestIsLiveVolumeTarget states the split the export ordering depends on.
func TestIsLiveVolumeTarget(t *testing.T) {
	assert.True(t, isLiveVolumeTarget(dev1alpha1.KindPVCShort))
	assert.True(t, isLiveVolumeTarget(dev1alpha1.KindVirtualDiskShort))
	assert.False(t, isLiveVolumeTarget(dev1alpha1.KindSnapshotShort))
	assert.False(t, isLiveVolumeTarget(dev1alpha1.KindVolumeSnapshotShort))
	assert.False(t, isLiveVolumeTarget(dev1alpha1.KindVirtualDiskSnapshotShort))
}

// TestSetConditionReadyServerReady covers the guard the condition carries: an export whose idle window has
// already closed must not be flipped back to Ready by a later pass.
func TestSetConditionReadyServerReady(t *testing.T) {
	t.Run("sets Ready when a condition is present", func(t *testing.T) {
		de := createDataExport(dev1alpha1.DataExportSpec{TTL: "10m"})
		de.Status.Conditions = []metav1.Condition{{
			Type:   string(common.ConditionReady),
			Status: metav1.ConditionFalse,
			Reason: string(common.ReasonPending),
		}}

		setConditionReadyServerReady(de)

		cond := findCondition(de.Status.Conditions, string(common.ConditionReady))
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, string(common.ReasonServerReady), cond.Reason)
	})

	t.Run("leaves an expired export alone", func(t *testing.T) {
		de := createDataExport(dev1alpha1.DataExportSpec{TTL: "10m"})
		de.Status.Conditions = []metav1.Condition{{
			Type:   string(common.ConditionReady),
			Status: metav1.ConditionFalse,
			Reason: string(common.ReasonExpired),
		}}

		setConditionReadyServerReady(de)

		cond := findCondition(de.Status.Conditions, string(common.ConditionReady))
		require.NotNil(t, cond)
		assert.Equal(t, string(common.ReasonExpired), cond.Reason, "an expired export must not be revived")
	})
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
