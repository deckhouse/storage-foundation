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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// blockingTeardownDataImport builds a PopulateData DataImport that already carries the storage-manager
// finalizer (as a prior successful reconcile would have added), staged for one of the two paths that
// invoke teardown and used to ignore its done/allGone report: deletion (DeletionTimestamp set) or
// idle-TTL expiry (serverState=IdleExpired).
func blockingTeardownDataImport(name string, deleting bool) *dev1alpha1.DataImport {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "ns",
			UID:        types.UID(name + "-uid"),
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec: dev1alpha1.DataImportSpec{Mode: dev1alpha1.DataImportModePopulateData, TTL: "30m"},
	}
	if deleting {
		now := fixedNow
		di.DeletionTimestamp = &now
	} else {
		di.Status.ServerState = string(common.ServerStateIdleExpired)
	}
	return di
}

// blockingTeardownCreatePVCDataImport builds a CreatePVC DataImport paired with its already-Bound target
// PVC (storage-manager annotations stamped, our finalizer present, as ensureImportPVC would have left it),
// staged for one of the two paths that invoke teardown and used to ignore its done/allGone report:
// deletion (DeletionTimestamp set) or idle-TTL expiry (serverState=IdleExpired). Mirrors
// blockingTeardownDataImport's PopulateData fixture, but CreatePVC additionally needs a live target PVC:
// teardownImportInfra/cleanupDataImport read it (getTargetPVC/isTargetPVCOwnedBy) even though this pair of
// branches never deletes it.
func blockingTeardownCreatePVCDataImport(name string, deleting bool) (*dev1alpha1.DataImport, *corev1.PersistentVolumeClaim) {
	di := createPVCImport(name, "")
	di.UID = types.UID(name + "-uid")
	di.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
	if deleting {
		now := fixedNow
		di.DeletionTimestamp = &now
	} else {
		di.Status.ServerState = string(common.ServerStateIdleExpired)
	}

	targetPVC := boundImportPVC(name)
	targetPVC.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
	return di, targetPVC
}

// TestReconcile_RequeuesWhileImporterPodBlocksTeardown is the direct regression guard for the bug where
// Reconcile discarded cleanupDataImport's/teardownImportInfra's done/allGone return value on the
// deletion and idle-TTL-expiry branches and always returned a zero ctrl.Result. Since this controller does
// not watch Pods (cmd/main.go), and teardownImportInfra now blocks internal-PVC deletion on the importer
// pod being fully gone, that discarded value meant: whenever a live importer pod blocked teardown, the
// object silently stalled until the manager's hourly SyncPeriod resync instead of self-requeuing. It
// exercises the real Reconcile (not cleanupDataImport/teardownImportInfra directly) on both affected
// branches, and then proves the requeue actually clears once the obstruction (the live pod) is gone.
//
// The CreatePVC rows extend the same guard to the scratch-PVC path this mode adds: a live importer pod
// must block deletion of the CreatePVC scratch PVC exactly as it blocks PopulateData's internal PVC, while
// the target PVC (the user's product) is never deleted and only ever loses our finalizer on the deletion
// branch, once teardown is fully done.
func TestReconcile_RequeuesWhileImporterPodBlocksTeardown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mode     dev1alpha1.DataImportMode
		deleting bool
	}{
		{name: "success: PopulateData deletion path requeues while the importer pod is live, then completes once it is gone", mode: dev1alpha1.DataImportModePopulateData, deleting: true},
		{name: "success: PopulateData idle-TTL expiry path requeues while the importer pod is live, then completes once it is gone", mode: dev1alpha1.DataImportModePopulateData, deleting: false},
		{name: "success: CreatePVC deletion path requeues while the importer pod blocks the scratch PVC teardown, then releases the target PVC's finalizer once it is gone", mode: dev1alpha1.DataImportModeCreatePVC, deleting: true},
		{name: "success: CreatePVC idle-TTL expiry path requeues while the importer pod blocks the scratch PVC teardown, then completes once it is gone, leaving the target PVC untouched", mode: dev1alpha1.DataImportModeCreatePVC, deleting: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			importName := "blocked-" + string(tt.mode) + "-deletion"
			if !tt.deleting {
				importName = "blocked-" + string(tt.mode) + "-expiry"
			}

			var di *dev1alpha1.DataImport
			var targetPVC *corev1.PersistentVolumeClaim
			objs := []client.Object{}

			switch tt.mode {
			case dev1alpha1.DataImportModePopulateData:
				di = blockingTeardownDataImport(importName, tt.deleting)
			case dev1alpha1.DataImportModeCreatePVC:
				di, targetPVC = blockingTeardownCreatePVCDataImport(importName, tt.deleting)
				objs = append(objs, targetPVC)
			}

			names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
			pod := importerPodFixture("importer-pod", flowControllerNamespace, names.DeployName)
			objs = append(objs, di, pod)

			var scratchPVCKey types.NamespacedName
			if tt.mode == dev1alpha1.DataImportModeCreatePVC {
				scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, flowControllerNamespace)
				scratchPVCKey = types.NamespacedName{Namespace: flowControllerNamespace, Name: names.ImportScratchPVCName}
				objs = append(objs, scratchPVC)
			}

			r, c, _ := newPopulateDataFlowReconciler(t, objs, nil)

			res, err := r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter,
				"a live importer pod blocking teardown must trigger an explicit self-requeue, not a zero Result")

			got := &dev1alpha1.DataImport{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
			assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName,
				"the finalizer must survive while a live importer pod blocks teardown")

			if tt.mode == dev1alpha1.DataImportModeCreatePVC {
				gotScratch := &corev1.PersistentVolumeClaim{}
				require.NoError(t, c.Get(context.Background(), scratchPVCKey, gotScratch),
					"the CreatePVC scratch PVC must survive while a live importer pod blocks teardown")

				gotTarget := &corev1.PersistentVolumeClaim{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: targetPVC.Namespace, Name: targetPVC.Name}, gotTarget))
				assert.Contains(t, gotTarget.Finalizers, dev1alpha1.StorageManagerFinalizerName,
					"the target PVC is never touched while teardown is blocked")
			}

			// The importer pod terminates (the kubelet finishes the unmount) -- the obstruction that made
			// teardownImportInfra/cleanupDataImport report done=false/allGone=false is now gone.
			require.NoError(t, c.Delete(context.Background(), pod))

			res, err = r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			assert.Zero(t, res.RequeueAfter,
				"teardown completes once the importer pod is gone, so Reconcile must stop self-requeuing")

			if tt.mode == dev1alpha1.DataImportModeCreatePVC {
				getErr := c.Get(context.Background(), scratchPVCKey, &corev1.PersistentVolumeClaim{})
				require.Error(t, getErr, "the scratch PVC must be deleted once the importer pod is gone")
				assert.True(t, apierrors.IsNotFound(getErr))
			}

			if tt.deleting {
				getErr := c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, &dev1alpha1.DataImport{})
				require.Error(t, getErr, "once the last finalizer is removed the deleting object is fully gone")
				assert.True(t, apierrors.IsNotFound(getErr))

				if tt.mode == dev1alpha1.DataImportModeCreatePVC {
					gotTarget := &corev1.PersistentVolumeClaim{}
					require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: targetPVC.Namespace, Name: targetPVC.Name}, gotTarget),
						"the target PVC is the user's product and must never be deleted by DataImport cleanup")
					assert.NotContains(t, gotTarget.Finalizers, dev1alpha1.StorageManagerFinalizerName,
						"our finalizer is released once the DataImport that owns it is fully cleaned up")
				}
			} else {
				got = &dev1alpha1.DataImport{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
				assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName,
					"idle-TTL expiry keeps the CR (and its finalizer) around for the garbage collector")
				assert.Equal(t, string(common.PhaseExpired), got.Status.Phase)

				if tt.mode == dev1alpha1.DataImportModeCreatePVC {
					gotTarget := &corev1.PersistentVolumeClaim{}
					require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: targetPVC.Namespace, Name: targetPVC.Name}, gotTarget),
						"idle-TTL expiry never touches the target PVC -- only the deletion branch releases its finalizer")
					assert.Contains(t, gotTarget.Finalizers, dev1alpha1.StorageManagerFinalizerName)
					assert.Equal(t, corev1.ClaimBound, gotTarget.Status.Phase)
				}
			}
		})
	}
}
