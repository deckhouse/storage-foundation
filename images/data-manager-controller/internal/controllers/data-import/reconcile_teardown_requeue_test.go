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
		Spec: dev1alpha1.DataImportSpec{Mode: dev1alpha1.DataImportModePopulateData, Ttl: "30m"},
	}
	if deleting {
		now := fixedNow
		di.DeletionTimestamp = &now
	} else {
		di.Status.ServerState = string(common.ServerStateIdleExpired)
	}
	return di
}

// TestReconcile_RequeuesWhileImporterPodBlocksTeardown is the direct regression guard for the bug where
// Reconcile discarded cleanupDataImport's/teardownImportInfra's done/allGone return value on the
// deletion and idle-TTL-expiry branches and always returned a zero ctrl.Result. Since this controller does
// not watch Pods (cmd/main.go), and teardownImportInfra now blocks internal-PVC deletion on the importer
// pod being fully gone, that discarded value meant: whenever a live importer pod blocked teardown, the
// object silently stalled until the manager's hourly SyncPeriod resync instead of self-requeuing. It
// exercises the real Reconcile (not cleanupDataImport/teardownImportInfra directly) on both affected
// branches, and then proves the requeue actually clears once the obstruction (the live pod) is gone.
func TestReconcile_RequeuesWhileImporterPodBlocksTeardown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		deleting bool
	}{
		{name: "success: deletion path requeues while the importer pod is live, then completes once it is gone", deleting: true},
		{name: "success: idle-TTL expiry path requeues while the importer pod is live, then completes once it is gone", deleting: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			importName := "blocked-deletion"
			if !tt.deleting {
				importName = "blocked-expiry"
			}
			di := blockingTeardownDataImport(importName, tt.deleting)
			names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
			pod := importerPodFixture("importer-pod", flowControllerNamespace, names.DeployName)

			r, c, _ := newPopulateDataFlowReconciler(t, []client.Object{di, pod}, nil)

			res, err := r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter,
				"a live importer pod blocking teardown must trigger an explicit self-requeue, not a zero Result")

			got := &dev1alpha1.DataImport{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
			assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName,
				"the finalizer must survive while a live importer pod blocks teardown")

			// The importer pod terminates (the kubelet finishes the unmount) -- the obstruction that made
			// teardownImportInfra/cleanupDataImport report done=false/allGone=false is now gone.
			require.NoError(t, c.Delete(context.Background(), pod))

			res, err = r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			assert.Zero(t, res.RequeueAfter,
				"teardown completes once the importer pod is gone, so Reconcile must stop self-requeuing")

			if tt.deleting {
				getErr := c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, &dev1alpha1.DataImport{})
				require.Error(t, getErr, "once the last finalizer is removed the deleting object is fully gone")
				assert.True(t, apierrors.IsNotFound(getErr))
			} else {
				got = &dev1alpha1.DataImport{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, got))
				assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName,
					"idle-TTL expiry keeps the CR (and its finalizer) around for the garbage collector")
				assert.Equal(t, string(common.PhaseExpired), got.Status.Phase)
			}
		})
	}
}
