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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// fakeGCManager is a minimal ReconcileGCManager over ConfigMap, so Reconcile can be exercised in
// isolation from any concrete resource kind. ListForDelete is unused by Reconcile.
type fakeGCManager struct {
	shouldDelete bool
}

func (m *fakeGCManager) New() client.Object                 { return &corev1.ConfigMap{} }
func (m *fakeGCManager) ShouldBeDeleted(client.Object) bool { return m.shouldDelete }
func (m *fakeGCManager) ListForDelete(context.Context, time.Time) ([]client.Object, error) {
	return nil, nil
}

// fakePreDeleteManager additionally implements the optional PreDeleter, recording whether the hook ran
// and returning a configurable error, so the pre-delete branch of Reconcile can be asserted.
type fakePreDeleteManager struct {
	fakeGCManager
	preDeleteErr    error
	preDeleteCalled bool
}

func (m *fakePreDeleteManager) PreDelete(context.Context, client.Object) error {
	m.preDeleteCalled = true
	return m.preDeleteErr
}

func gcTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func cmObj(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}}
}

func cmDeleting(name string) *corev1.ConfigMap {
	c := cmObj(name)
	ts := metav1.NewTime(time.Now().Add(-time.Minute))
	c.DeletionTimestamp = &ts
	c.Finalizers = []string{"test/finalizer"} // a non-nil DeletionTimestamp is only valid with a finalizer
	return c
}

func gcReq(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: name}}
}

func mustBeGone(t *testing.T, c client.Client, name string) {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, &corev1.ConfigMap{})
	assert.True(t, apierrors.IsNotFound(err), "object %q must be deleted, got err=%v", name, err)
}

func mustExist(t *testing.T, c client.Client, name string) {
	t.Helper()
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, &corev1.ConfigMap{}),
		"object %q must still exist", name)
}

func TestReconciler_Reconcile(t *testing.T) {
	scheme := gcTestScheme(t)
	ctx := context.Background()

	t.Run("ShouldBeDeleted=true, not terminating → deleted", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cmObj("reap")).Build()
		r := NewReconciler(c, nil, &fakeGCManager{shouldDelete: true})
		res, err := r.Reconcile(ctx, gcReq("reap"))
		require.NoError(t, err)
		assert.Equal(t, reconcile.Result{}, res)
		mustBeGone(t, c, "reap")
	})

	t.Run("ShouldBeDeleted=false → no-op, kept", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cmObj("keep")).Build()
		r := NewReconciler(c, nil, &fakeGCManager{shouldDelete: false})
		_, err := r.Reconcile(ctx, gcReq("keep"))
		require.NoError(t, err)
		mustExist(t, c, "keep")
	})

	t.Run("already terminating → no-op even when ShouldBeDeleted", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cmDeleting("dying")).Build()
		// Use a PreDeleter manager and assert the hook is NEVER reached: the terminating guard returns
		// before ShouldBeDeleted/PreDelete/Delete. (Asserting only "object still exists" would be vacuous —
		// the finalizer required to keep a fake DeletionTimestamp valid also blocks Delete regardless of the
		// guard, so a removed guard would still leave the object present. preDeleteCalled==false is what
		// actually pins the short-circuit.)
		m := &fakePreDeleteManager{fakeGCManager: fakeGCManager{shouldDelete: true}}
		r := NewReconciler(c, nil, m)
		_, err := r.Reconcile(ctx, gcReq("dying"))
		require.NoError(t, err)
		assert.False(t, m.preDeleteCalled, "terminating guard must short-circuit before any delete work")
		mustExist(t, c, "dying")
	})

	t.Run("object absent → nil error, no-op", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := NewReconciler(c, nil, &fakeGCManager{shouldDelete: true})
		res, err := r.Reconcile(ctx, gcReq("ghost"))
		require.NoError(t, err)
		assert.Equal(t, reconcile.Result{}, res)
	})

	t.Run("PreDelete error → object NOT deleted, error requeued", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cmObj("guard")).Build()
		m := &fakePreDeleteManager{fakeGCManager: fakeGCManager{shouldDelete: true}, preDeleteErr: errors.New("pre-delete boom")}
		r := NewReconciler(c, nil, m)
		_, err := r.Reconcile(ctx, gcReq("guard"))
		require.Error(t, err)
		assert.True(t, m.preDeleteCalled, "PreDelete must have been attempted")
		mustExist(t, c, "guard") // a failed PreDelete aborts the delete
	})

	t.Run("PreDelete success → hook ran, then deleted", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cmObj("hooked")).Build()
		m := &fakePreDeleteManager{fakeGCManager: fakeGCManager{shouldDelete: true}}
		r := NewReconciler(c, nil, m)
		_, err := r.Reconcile(ctx, gcReq("hooked"))
		require.NoError(t, err)
		assert.True(t, m.preDeleteCalled, "PreDelete hook must run before the delete")
		mustBeGone(t, c, "hooked")
	})
}
