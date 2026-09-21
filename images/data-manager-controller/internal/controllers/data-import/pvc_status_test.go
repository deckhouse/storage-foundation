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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

func pvcStatusScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

// TestInternalPVCStatus covers the classification of the internal scratch PVC, which never consults the
// API (unlike CheckPVCStatus/processPVCPendingStatus, which may need to look up the StorageClass).
func TestInternalPVCStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		phase corev1.PersistentVolumeClaimPhase
		want  TargetStatus
	}{
		{
			name:  "Bound is Ready",
			phase: corev1.ClaimBound,
			want:  TargetStatusReady,
		},
		{
			name:  "Pending is Pending",
			phase: corev1.ClaimPending,
			want:  TargetStatusPending,
		},
		{
			name:  "Lost is Failed",
			phase: corev1.ClaimLost,
			want:  TargetStatusFailed,
		},
		{
			// The fake client never defaults status.phase on Create (unlike a real apiserver, which sets
			// Pending), so flow tests need pendingOnPVCCreateInterceptor to model that -- this empty-phase
			// classification is exactly why.
			name:  "empty phase is Failed",
			phase: "",
			want:  TargetStatusFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pvc := &corev1.PersistentVolumeClaim{
				Status: corev1.PersistentVolumeClaimStatus{Phase: tt.phase},
			}
			assert.Equal(t, tt.want, internalPVCStatus(pvc))
		})
	}
}

// ensurePVCTemplate builds a minimal PVC template for exercising EnsurePVC.
func ensurePVCTemplate(name string, size string) *dev1alpha1.PersistentVolumeClaimTemplateSpec {
	sc := "fast"
	fs := dev1alpha1.PersistentVolumeFilesystem
	return &dev1alpha1.PersistentVolumeClaimTemplateSpec{
		PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: name},
		PersistentVolumeClaimSpec: dev1alpha1.PersistentVolumeClaimSpec{
			AccessModes:      []dev1alpha1.PersistentVolumeAccessMode{dev1alpha1.ReadWriteOnce},
			Resources:        dev1alpha1.VolumeResourceRequirements{Requests: dev1alpha1.ResourceList{dev1alpha1.ResourceStorage: resource.MustParse(size)}},
			StorageClassName: &sc,
			VolumeMode:       &fs,
		},
	}
}

// TestEnsurePVC_ReturnsCurrentPVC pins EnsurePVC's contract: it always returns the current object, whether
// freshly created, unchanged, or updated -- callers no longer need a separate Get after calling it.
func TestEnsurePVC_ReturnsCurrentPVC(t *testing.T) {
	t.Parallel()

	resourceName := types.NamespacedName{Namespace: "owner-ns", Name: "owner-di"}

	t.Run("PVC does not exist: create and return it", func(t *testing.T) {
		t.Parallel()

		c := fake.NewClientBuilder().WithScheme(pvcStatusScheme(t)).Build()
		tmpl := ensurePVCTemplate("new-pvc", "1Gi")

		got, err := EnsurePVC(context.Background(), c, "pvc-ns", resourceName, tmpl, nil)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "new-pvc", got.Name)
		assert.Equal(t, "pvc-ns", got.Namespace)
		assert.Equal(t, dev1alpha1.LabelDataImportValue, got.Labels[dev1alpha1.LabelApplicationKey])
		assert.Equal(t, resourceName.Namespace, got.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey])
		assert.Equal(t, resourceName.Name, got.Annotations[dev1alpha1.AnnotationStorageManagerNameKey])
		assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName)
	})

	t.Run("PVC exists and matches spec: return it, no Update call", func(t *testing.T) {
		t.Parallel()

		tmpl := ensurePVCTemplate("existing-pvc", "2Gi")
		existing := makePVC(tmpl, nil, "pvc-ns", resourceName)

		var updateCalls int
		c := fake.NewClientBuilder().WithScheme(pvcStatusScheme(t)).WithObjects(existing).
			WithInterceptorFuncs(interceptor.Funcs{
				Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					updateCalls++
					return cl.Update(ctx, obj, opts...)
				},
			}).Build()

		got, err := EnsurePVC(context.Background(), c, "pvc-ns", resourceName, tmpl, nil)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "existing-pvc", got.Name)
		assert.Equal(t, 0, updateCalls, "an in-sync PVC must not be updated")
	})

	t.Run("PVC exists with a different spec: update and return the new spec", func(t *testing.T) {
		t.Parallel()

		oldTmpl := ensurePVCTemplate("changed-pvc", "1Gi")
		existing := makePVC(oldTmpl, nil, "pvc-ns", resourceName)

		newTmpl := ensurePVCTemplate("changed-pvc", "5Gi")

		c := fake.NewClientBuilder().WithScheme(pvcStatusScheme(t)).WithObjects(existing).Build()

		got, err := EnsurePVC(context.Background(), c, "pvc-ns", resourceName, newTmpl, nil)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.True(t, got.Spec.Resources.Requests[corev1.ResourceStorage].Equal(resource.MustParse("5Gi")))

		persisted := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "pvc-ns", Name: "changed-pvc"}, persisted))
		assert.True(t, persisted.Spec.Resources.Requests[corev1.ResourceStorage].Equal(resource.MustParse("5Gi")))
	})
}
