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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// pvHandoverTestScheme is the minimal typed scheme this file's fake.Client fixtures need: only
// PersistentVolume and PersistentVolumeClaim ever appear here.
func pvHandoverTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

// pvHandoverTestTargetPVC builds a live target PVC with a fully populated spec, standing in for what
// the apiserver hands back after ensureImportPVC creates or reads it.
func pvHandoverTestTargetPVC(mutate func(*corev1.PersistentVolumeClaim)) *corev1.PersistentVolumeClaim {
	storageClass := "fast"
	volumeMode := corev1.PersistentVolumeFilesystem
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "target",
			Namespace: "user-ns",
			UID:       "target-uid",
			Labels:    map[string]string{"app": "data-importer"},
			Annotations: map[string]string{
				"storage-manager-namespace": "user-ns",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			VolumeMode:       &volumeMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
	}
	if mutate != nil {
		mutate(pvc)
	}
	return pvc
}

func TestComputeCreatePVCScratchTemplate(t *testing.T) {
	t.Parallel()

	t.Run("success: mirrors the live target PVC", func(t *testing.T) {
		t.Parallel()

		target := pvHandoverTestTargetPVC(nil)
		got := computeCreatePVCScratchTemplate("scratch-name", target)

		require.NotNil(t, got)
		assert.Equal(t, "scratch-name", got.PersistentVolumeClaimTemplateMetadata.Name)
		assert.Equal(t, []dev1alpha1.PersistentVolumeAccessMode{dev1alpha1.PersistentVolumeAccessMode(corev1.ReadWriteOnce)}, got.AccessModes)
		require.NotNil(t, got.StorageClassName)
		assert.Equal(t, "fast", *got.StorageClassName)
		require.NotNil(t, got.VolumeMode)
		assert.Equal(t, dev1alpha1.PersistentVolumeMode(corev1.PersistentVolumeFilesystem), *got.VolumeMode)
		gotQty, ok := got.Resources.Requests[dev1alpha1.ResourceStorage]
		require.True(t, ok)
		assert.Equal(t, "10Gi", gotQty.String())

		// None of the target's own metadata leaks into the scratch template.
		assert.Empty(t, got.PersistentVolumeClaimTemplateMetadata.Labels)
	})

	t.Run("success: RWX access modes are copied", func(t *testing.T) {
		t.Parallel()

		target := pvHandoverTestTargetPVC(func(pvc *corev1.PersistentVolumeClaim) {
			pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
				corev1.ReadOnlyMany,
			}
		})
		got := computeCreatePVCScratchTemplate("scratch-name", target)

		assert.Equal(t, []dev1alpha1.PersistentVolumeAccessMode{
			dev1alpha1.PersistentVolumeAccessMode(corev1.ReadWriteMany),
			dev1alpha1.PersistentVolumeAccessMode(corev1.ReadOnlyMany),
		}, got.AccessModes)
	})

	t.Run("success: output does not alias the input", func(t *testing.T) {
		t.Parallel()

		target := pvHandoverTestTargetPVC(nil)
		got := computeCreatePVCScratchTemplate("scratch-name", target)

		// Mutate every reference type the function copies from, and assert the target is untouched.
		got.AccessModes[0] = dev1alpha1.PersistentVolumeAccessMode(corev1.ReadWriteMany)
		got.Resources.Requests[dev1alpha1.ResourceStorage] = resource.MustParse("999Gi")
		*got.StorageClassName = "mutated"
		*got.VolumeMode = dev1alpha1.PersistentVolumeMode(corev1.PersistentVolumeBlock)

		assert.Equal(t, corev1.ReadWriteOnce, target.Spec.AccessModes[0])
		targetQty := target.Spec.Resources.Requests[corev1.ResourceStorage]
		assert.Equal(t, "10Gi", targetQty.String())
		assert.Equal(t, "fast", *target.Spec.StorageClassName)
		assert.Equal(t, corev1.PersistentVolumeFilesystem, *target.Spec.VolumeMode)
	})

	t.Run("success: nil pointers stay nil", func(t *testing.T) {
		t.Parallel()

		target := pvHandoverTestTargetPVC(func(pvc *corev1.PersistentVolumeClaim) {
			pvc.Spec.StorageClassName = nil
			pvc.Spec.VolumeMode = nil
			pvc.Spec.AccessModes = nil
			pvc.Spec.Resources.Requests = nil
		})
		got := computeCreatePVCScratchTemplate("scratch-name", target)

		assert.Nil(t, got.StorageClassName)
		assert.Nil(t, got.VolumeMode)
		assert.Empty(t, got.AccessModes)
		assert.Empty(t, got.Resources.Requests)
	})
}

func TestIsPVClaimedBy(t *testing.T) {
	t.Parallel()

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "user-ns", UID: "target-uid"},
	}

	tests := []struct {
		name     string
		claimRef *corev1.ObjectReference
		want     bool
	}{
		{
			name:     "success: match with UID",
			claimRef: &corev1.ObjectReference{Namespace: "user-ns", Name: "target", UID: "target-uid"},
			want:     true,
		},
		{
			name:     "success: match with empty UID is a pre-bound reference and counts as a match",
			claimRef: &corev1.ObjectReference{Namespace: "user-ns", Name: "target"},
			want:     true,
		},
		{
			name:     "error: nil claimRef is not a match",
			claimRef: nil,
			want:     false,
		},
		{
			name:     "error: other name is not a match",
			claimRef: &corev1.ObjectReference{Namespace: "user-ns", Name: "someone-else", UID: "target-uid"},
			want:     false,
		},
		{
			name:     "error: other namespace is not a match",
			claimRef: &corev1.ObjectReference{Namespace: "other-ns", Name: "target", UID: "target-uid"},
			want:     false,
		},
		{
			name:     "error: same name and namespace but a different UID is not a match",
			claimRef: &corev1.ObjectReference{Namespace: "user-ns", Name: "target", UID: "someone-elses-uid"},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pv := &corev1.PersistentVolume{
				Spec: corev1.PersistentVolumeSpec{ClaimRef: tt.claimRef},
			}

			got := isPVClaimedBy(pv, pvc)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestComputeScratchNodeHint is the guard on the CreatePVC node-hint gate: the wait condition is a
// conjunction of the StorageClass's binding mode and spec.waitForFirstConsumer, and gating on either
// alone is wrong in a different way. All four binding-mode x flag combinations are their own subtest,
// named for the combination, and the WFC rows are additionally split on whether the scheduler's
// selected-node annotation is present.
func TestComputeScratchNodeHint(t *testing.T) {
	t.Parallel()

	t.Run("success: Immediate class and waitForFirstConsumer=false proceeds with no hint", func(t *testing.T) {
		t.Parallel()

		target := pvHandoverTestTargetPVC(nil)
		node, wait := computeScratchNodeHint(target, false, false)
		assert.Empty(t, node)
		assert.False(t, wait)
	})

	t.Run("success: Immediate class and waitForFirstConsumer=true proceeds immediately with no hint", func(t *testing.T) {
		t.Parallel()

		// This is the sharp edge that makes the conjunction necessary: an Immediate-bound claim
		// never gets a selected-node annotation from the scheduler, so gating on
		// waitForFirstConsumer alone would wait forever for something that never arrives.
		target := pvHandoverTestTargetPVC(nil)
		node, wait := computeScratchNodeHint(target, false, true)
		assert.Empty(t, node)
		assert.False(t, wait, "an Immediate class must never wait for a selected-node annotation")
	})

	t.Run("success: WaitForFirstConsumer class and waitForFirstConsumer=false proceeds with no hint", func(t *testing.T) {
		t.Parallel()

		target := pvHandoverTestTargetPVC(func(pvc *corev1.PersistentVolumeClaim) {
			pvc.Annotations[annSelectedNode] = "node-a"
		})
		node, wait := computeScratchNodeHint(target, true, false)
		assert.Empty(t, node, "the importer pod is its own first consumer; no hint is needed even if one is present")
		assert.False(t, wait)
	})

	t.Run("WaitForFirstConsumer class and waitForFirstConsumer=true", func(t *testing.T) {
		t.Parallel()

		t.Run("success: annotation absent waits", func(t *testing.T) {
			t.Parallel()

			target := pvHandoverTestTargetPVC(nil)
			node, wait := computeScratchNodeHint(target, true, true)
			assert.Empty(t, node)
			assert.True(t, wait)
		})

		t.Run("success: annotation present returns the hint", func(t *testing.T) {
			t.Parallel()

			target := pvHandoverTestTargetPVC(func(pvc *corev1.PersistentVolumeClaim) {
				pvc.Annotations[annSelectedNode] = "node-a"
			})
			node, wait := computeScratchNodeHint(target, true, true)
			assert.Equal(t, "node-a", node)
			assert.False(t, wait)
		})
	})
}

// pvHandoverTestPV builds a bound PersistentVolume whose claimRef names claimant, carrying apiVersion
// and kind the way the binder itself writes them.
func pvHandoverTestPV(claimant *corev1.PersistentVolumeClaim) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{
				APIVersion:      "v1",
				Kind:            "PersistentVolumeClaim",
				Namespace:       claimant.Namespace,
				Name:            claimant.Name,
				UID:             claimant.UID,
				ResourceVersion: claimant.ResourceVersion,
			},
		},
	}
}

func TestPatchPVClaimRefToTargetIsIdempotent(t *testing.T) {
	t.Parallel()

	t.Run("success: pv already claimed by the target PVC produces no write", func(t *testing.T) {
		t.Parallel()

		target := pvHandoverTestTargetPVC(nil)
		pv := pvHandoverTestPV(target)
		scheme := pvHandoverTestScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).Build()

		stored := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, stored))
		base := stored.DeepCopy()
		resourceVersionBefore := stored.ResourceVersion

		err := patchPVClaimRefToTarget(context.Background(), c, stored, base, target)
		require.NoError(t, err)

		after := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, after))
		assert.Equal(t, resourceVersionBefore, after.ResourceVersion, "an already-idempotent claimRef must not trigger a write")
	})

	t.Run("success: apiVersion and kind survive the merge patch", func(t *testing.T) {
		t.Parallel()

		target := pvHandoverTestTargetPVC(nil)
		scratch := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "scratch", Namespace: "d8", UID: "scratch-uid"},
		}
		// The fixture PV starts out claimed by the scratch PVC, carrying apiVersion/kind the way
		// the binder writes them -- a plain client.MergeFrom sends any field present in base but
		// omitted from the modified object as an explicit null, so these two fields would be wiped
		// by a patch literal that leaves them empty.
		pv := pvHandoverTestPV(scratch)

		scheme := pvHandoverTestScheme(t)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).Build()

		stored := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, stored))
		base := stored.DeepCopy()

		err := patchPVClaimRefToTarget(context.Background(), c, stored, base, target)
		require.NoError(t, err)

		after := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, after))
		require.NotNil(t, after.Spec.ClaimRef)
		assert.Equal(t, "v1", after.Spec.ClaimRef.APIVersion)
		assert.Equal(t, "PersistentVolumeClaim", after.Spec.ClaimRef.Kind)
		assert.Equal(t, target.Namespace, after.Spec.ClaimRef.Namespace)
		assert.Equal(t, target.Name, after.Spec.ClaimRef.Name)
		assert.Equal(t, target.UID, after.Spec.ClaimRef.UID)
		assert.Equal(t, target.ResourceVersion, after.Spec.ClaimRef.ResourceVersion)
	})
}

// TestPatchPVClaimRefToTargetUsesOptimisticLock pins that claimRef, a multi-writer field, is patched
// with an optimistic lock: a stale base must surface apierrors.IsConflict out of the helper rather than
// being retried internally or silently dropping whichever writer raced it.
func TestPatchPVClaimRefToTargetUsesOptimisticLock(t *testing.T) {
	t.Parallel()

	target := pvHandoverTestTargetPVC(nil)
	scratch := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "scratch", Namespace: "d8", UID: "scratch-uid"},
	}
	pv := pvHandoverTestPV(scratch)

	scheme := pvHandoverTestScheme(t)
	calls := 0
	conflictErr := apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "persistentvolumes"}, pv.Name, errors.New("conflict"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
				calls++
				return conflictErr
			},
		}).Build()

	stored := &corev1.PersistentVolume{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, stored))
	// base is deliberately not re-fetched after some concurrent writer's edit -- it stands in for a
	// stale snapshot taken before another actor changed the object, which is exactly what the
	// interceptor models by rejecting the patch as a conflict.
	base := stored.DeepCopy()

	err := patchPVClaimRefToTarget(context.Background(), c, stored, base, target)
	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err), "a conflict on the claimRef patch must surface as apierrors.IsConflict")
	assert.Equal(t, 1, calls, "patchPVClaimRefToTarget must not retry the conflicted patch internally")
}
