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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	scheme := runtime.NewScheme()
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	b := fake.NewClientBuilder().WithScheme(scheme)
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
