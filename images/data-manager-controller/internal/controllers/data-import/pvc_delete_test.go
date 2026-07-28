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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

func deleteScratchPVCScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

// TestDeleteScratchPVC covers the scratch-PVC teardown helper's idempotency contract: it must succeed
// (and leave the PVC gone) whether the finalizer is still present, already absent, or the PVC itself is
// already gone by the time it is called.
func TestDeleteScratchPVC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		withObject        bool // whether the PVC is seeded into the fake client's store
		finalizers        []string
		deletionTimestamp bool // simulate a concurrent user-initiated `kubectl delete` racing our own cleanup
	}{
		{
			name:       "finalizer present",
			withObject: true,
			finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		{
			name:       "no finalizer",
			withObject: true,
			finalizers: nil,
		},
		{
			name:       "PVC already gone",
			withObject: false,
			finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		{
			name:              "finalizer present and DeletionTimestamp already set by a concurrent delete",
			withObject:        true,
			finalizers:        []string{dev1alpha1.StorageManagerFinalizerName},
			deletionTimestamp: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := deleteScratchPVCScheme(t)
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "imp-1",
					Namespace:  "ns",
					Finalizers: tt.finalizers,
				},
			}
			if tt.deletionTimestamp {
				now := metav1.NewTime(time.Now())
				pvc.DeletionTimestamp = &now
			}

			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.withObject {
				builder = builder.WithObjects(pvc)
			}
			c := builder.Build()

			err := DeleteScratchPVC(context.Background(), c, pvc)
			require.NoError(t, err)

			got := &corev1.PersistentVolumeClaim{}
			getErr := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "imp-1"}, got)
			require.Error(t, getErr)
			assert.True(t, apierrors.IsNotFound(getErr), "scratch PVC must be gone from the client after DeleteScratchPVC")
		})
	}
}

// TestDeleteScratchPVC_PatchConflictIsNotRetried pins the accepted single-shot, best-effort design
// documented in the fix plan: DeleteScratchPVC does not internally retry a conflict on the
// finalizer-strip Patch. The caller (ensureDataArtifact) treats any error here as log-only and
// non-fatal, so retrying internally would add complexity without changing observable behavior, and
// the sticky Completed terminal guard means the whole call is not naturally retried on a later
// reconcile either -- this test documents that a conflict surfaces as an error rather than being
// silently absorbed or retried.
func TestDeleteScratchPVC_PatchConflictIsNotRetried(t *testing.T) {
	t.Parallel()

	scheme := deleteScratchPVCScheme(t)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "imp-1",
			Namespace:  "ns",
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
	}

	calls := 0
	conflictErr := apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "persistentvolumeclaims"}, pvc.Name, errors.New("conflict"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				calls++
				return conflictErr
			},
		}).Build()

	err := DeleteScratchPVC(context.Background(), c, pvc)
	require.Error(t, err, "a conflict on the finalizer-strip Patch must surface, not be swallowed")
	assert.True(t, apierrors.IsConflict(err))
	assert.Equal(t, 1, calls, "DeleteScratchPVC must not internally retry the conflicted Patch -- retrying is the caller's/next-reconcile's responsibility, not this helper's")

	// The PVC must still exist: the Delete call is never reached once the finalizer-strip Patch fails.
	got := &corev1.PersistentVolumeClaim{}
	getErr := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "imp-1"}, got)
	require.NoError(t, getErr, "PVC must remain present when the finalizer-strip Patch fails before Delete is attempted")
}
