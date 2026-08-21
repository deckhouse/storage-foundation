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

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// TestShouldSetServerState is the monotonic state-machine guard that prevents an out-of-order or
// duplicate signal from corrupting a terminal serverState. The critical regression it protects
// (historically svdm 0.1.24/0.1.25): a genuine client completion (Finished) must never be clobbered by a
// later idle-expiry, or a completed import would be reported as abandoned and hang forever.
func TestShouldSetServerState(t *testing.T) {
	const (
		unset    = common.ServerState("")
		ready    = common.ServerStateReady
		finished = common.ServerStateFinished
		idle     = common.ServerStateIdleExpired
	)

	tests := []struct {
		current, next common.ServerState
		want          bool
	}{
		// Ready is only set from the unset state (safe to re-run on pod restart).
		{unset, ready, true},
		{ready, ready, false},
		{finished, ready, false},
		{idle, ready, false},
		// Finished always wins (authoritative for data integrity), idempotent against itself.
		{unset, finished, true},
		{ready, finished, true},
		{idle, finished, true},
		{finished, finished, false},
		// IdleExpired from unset/Ready only — MUST NOT clobber a Finished import (the core regression).
		{unset, idle, true},
		{ready, idle, true},
		{finished, idle, false}, // <-- completed import must not be reported abandoned
		{idle, idle, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.current)+"->"+string(tt.next), func(t *testing.T) {
			assert.Equal(t, tt.want, shouldSetServerState(tt.current, tt.next))
		})
	}
}

func newRepoClient(t *testing.T, objs ...*dev1alpha1.DataImport) *Client {
	t.Helper()
	return newRepoClientWithInterceptors(t, interceptor.Funcs{}, objs...)
}

// newRepoClientWithInterceptors builds the same fake-backed Client as newRepoClient with funcs layered on
// top, which is how a test injects an API-server answer the fake client would not produce on its own — a
// conflict on a status write, for instance.
func newRepoClientWithInterceptors(t *testing.T, funcs interceptor.Funcs, objs ...*dev1alpha1.DataImport) *Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	b := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(funcs)
	for _, o := range objs {
		b = b.WithStatusSubresource(o).WithObjects(o)
	}
	return &Client{Client: b.Build()}
}

func getDI(t *testing.T, c *Client, ns, name string) *dev1alpha1.DataImport {
	t.Helper()
	di := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, di))
	return di
}

// TestSetServerState_FinishedIsDurableAndPreservesControllerFields verifies the POST /finished guarantee:
// serverState=Finished is persisted, and the controller-owned status fields written concurrently
// (phase/conditions) are preserved rather than clobbered (SetServerState re-GETs the latest object).
func TestSetServerState_FinishedIsDurableAndPreservesControllerFields(t *testing.T) {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"},
		Status: dev1alpha1.DataExportImportStatus{
			Phase:           string(common.PhaseReady),
			ServerState:     string(common.ServerStateReady),
			AccessTimestamp: metav1.NewTime(time.Unix(1000, 0)),
			Conditions: []metav1.Condition{
				{Type: string(common.ConditionReady), Status: metav1.ConditionTrue, Reason: string(common.ReasonServerReady), LastTransitionTime: metav1.Now()},
			},
		},
	}
	c := newRepoClient(t, di)

	require.NoError(t, c.SetServerState(context.Background(), common.OperationImport, "ns", "imp", common.ServerStateFinished))

	got := getDI(t, c, "ns", "imp")
	assert.Equal(t, string(common.ServerStateFinished), got.Status.ServerState, "Finished must be persisted")
	// Controller-owned fields untouched by the pod write.
	assert.Equal(t, string(common.PhaseReady), got.Status.Phase)
	assert.Equal(t, metav1.NewTime(time.Unix(1000, 0)), got.Status.AccessTimestamp)
	assert.Len(t, got.Status.Conditions, 1)
}

// TestSetServerState_IdleExpiredDoesNotClobberFinished is the direct regression guard: once a client has
// finished, a later idle-expiry signal must be a no-op (the import completed; it is not abandoned).
func TestSetServerState_IdleExpiredDoesNotClobberFinished(t *testing.T) {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"},
		Status:     dev1alpha1.DataExportImportStatus{ServerState: string(common.ServerStateFinished)},
	}
	c := newRepoClient(t, di)

	require.NoError(t, c.SetServerState(context.Background(), common.OperationImport, "ns", "imp", common.ServerStateIdleExpired))

	assert.Equal(t, string(common.ServerStateFinished), getDI(t, c, "ns", "imp").Status.ServerState,
		"IdleExpired must not overwrite a Finished import")
}

// TestUpdateAccessTimestamp verifies the heartbeat writes status.accessTimestamp.
func TestUpdateAccessTimestamp(t *testing.T) {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"},
		Status:     dev1alpha1.DataExportImportStatus{ServerState: string(common.ServerStateReady)},
	}
	c := newRepoClient(t, di)

	ts := time.Unix(5000, 0)
	require.NoError(t, c.UpdateAccessTimestamp(context.Background(), common.OperationImport, "ns", "imp", ts))

	assert.Equal(t, metav1.NewTime(ts), getDI(t, c, "ns", "imp").Status.AccessTimestamp)
}

// TestUpdateAccessTimestamp_SurvivesLostRaceWithConcurrentStatusWrite covers the race the pod loses at
// startup. The pod publishes the very first accessTimestamp right after the data server starts, which is
// exactly when the controller is still writing phase and conditions of the same object, so the pod's
// status update is answered 409 Conflict. That single conflict is not one lost heartbeat: the next
// publication only happens on a tick that saw bytes move, so on a transfer nobody ever connects to there
// is no second attempt at all and the field stays unset for the whole life of the object. The write must
// therefore re-read the object and land, keeping the fields the other writer put there.
func TestUpdateAccessTimestamp_SurvivesLostRaceWithConcurrentStatusWrite(t *testing.T) {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"},
		Status:     dev1alpha1.DataExportImportStatus{ServerState: string(common.ServerStateReady)},
	}

	statusUpdates := 0
	c := newRepoClientWithInterceptors(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, inner client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			statusUpdates++
			if statusUpdates == 1 {
				// The other writer lands first and bumps resourceVersion, so the write below is refused
				// with a real conflict instead of a hand-made error.
				concurrent := &dev1alpha1.DataImport{}
				require.NoError(t, inner.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "imp"}, concurrent))
				concurrent.Status.Phase = string(common.PhaseReady)
				require.NoError(t, inner.Status().Update(ctx, concurrent))
			}
			return inner.SubResource(subResource).Update(ctx, obj, opts...)
		},
	}, di)

	ts := time.Unix(5000, 0)
	require.NoError(t, c.UpdateAccessTimestamp(context.Background(), common.OperationImport, "ns", "imp", ts))

	got := getDI(t, c, "ns", "imp")
	assert.Greater(t, statusUpdates, 1, "the refused write must be attempted again, not dropped")
	assert.Equal(t, metav1.NewTime(ts), got.Status.AccessTimestamp, "a lost race must not cost the timestamp")
	assert.Equal(t, string(common.PhaseReady), got.Status.Phase,
		"the retry must re-read the object, so the concurrent writer's field survives")
}

// TestUpdateAccessTimestamp_ReportsConflictThatNeverClears is the other half of the retry: when the
// conflict does not clear, the failure has to come back to the caller, which logs it. A retry loop that
// ends in a silent success would hide exactly the loss this write is retried for, and no other signal of
// it exists — the field simply stays empty.
func TestUpdateAccessTimestamp_ReportsConflictThatNeverClears(t *testing.T) {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"},
		Status:     dev1alpha1.DataExportImportStatus{ServerState: string(common.ServerStateReady)},
	}

	statusUpdates := 0
	c := newRepoClientWithInterceptors(t, interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
			statusUpdates++
			return apierrors.NewConflict(
				dev1alpha1.GroupVersion.WithResource(AuthzResourceImport).GroupResource(),
				obj.GetName(),
				errors.New("the object has been modified"),
			)
		},
	}, di)

	err := c.UpdateAccessTimestamp(context.Background(), common.OperationImport, "ns", "imp", time.Unix(5000, 0))

	require.Error(t, err, "an exhausted retry must not be reported as a successful publication")
	assert.True(t, apierrors.IsConflict(err), "the caller logs what it gets, so the conflict itself must reach it: %v", err)
	assert.Greater(t, statusUpdates, 1, "one conflict must not end the attempt")
	assert.True(t, getDI(t, c, "ns", "imp").Status.AccessTimestamp.IsZero(), "nothing was written, so nothing must be stored")
}
