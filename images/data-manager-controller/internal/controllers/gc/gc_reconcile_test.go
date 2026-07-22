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

package gc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	commongc "github.com/deckhouse/storage-foundation/common/gc"
)

func gcReconcileReq(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: name}}
}

// TestDataImportGC_ReconcileThroughManager composes the real dataImportGCManager with the generic
// common/gc Reconciler and a fake client, proving end-to-end at unit level that a reconcile tick actually
// deletes an aged terminal DataImport through the real ShouldBeDeleted filter and retains a fresh one.
func TestDataImportGC_ReconcileThroughManager(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dev1alpha1.AddToScheme(scheme))

	old := gcNow.Add(-25 * time.Hour)
	fresh := gcNow.Add(-time.Hour)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		di("aged", common.PhaseCompleted, &old, false),
		di("young", common.PhaseCompleted, &fresh, false),
	).Build()
	m := &dataImportGCManager{client: c, ttl: gcTTL, now: func() time.Time { return gcNow }}
	r := commongc.NewReconciler(c, nil, m)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, gcReconcileReq("aged"))
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "aged"}, &dev1alpha1.DataImport{})),
		"aged terminal DataImport must be collected")

	_, err = r.Reconcile(ctx, gcReconcileReq("young"))
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "young"}, &dev1alpha1.DataImport{}),
		"fresh terminal DataImport must be retained")
}

func TestDataExportGC_ReconcileThroughManager(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dev1alpha1.AddToScheme(scheme))

	old := gcNow.Add(-25 * time.Hour)
	fresh := gcNow.Add(-time.Hour)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		de("aged", common.PhaseExpired, &old, false),
		de("young", common.PhaseExpired, &fresh, false),
	).Build()
	m := &dataExportGCManager{client: c, ttl: gcTTL, now: func() time.Time { return gcNow }}
	r := commongc.NewReconciler(c, nil, m)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, gcReconcileReq("aged"))
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "aged"}, &dev1alpha1.DataExport{})),
		"aged terminal DataExport must be collected")

	_, err = r.Reconcile(ctx, gcReconcileReq("young"))
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "young"}, &dev1alpha1.DataExport{}),
		"fresh terminal DataExport must be retained")
}
