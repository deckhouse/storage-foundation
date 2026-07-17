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

package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	commongc "github.com/deckhouse/storage-foundation/common/gc"
)

func vcrReconcileReq(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: name}}
}

// TestVCRGC_ReconcileThroughManager composes the real vcrGCManager (which implements PreDeleter) with the
// generic Reconciler and a fake client, proving the full Reconcile → PreDelete → Delete path end-to-end:
// the aged VCR points at an ORPHAN artifact (a PersistentVolume with no ownerReferences) that only its
// PreDelete hook can reap, so asserting the artifact is gone after Reconcile pins that PreDelete actually
// ran before the delete (a plain delete without the hook would leak the orphan). A fresh VCR is retained.
func TestVCRGC_ReconcileThroughManager(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	old := vcrNow.Add(-25 * time.Hour)
	fresh := vcrNow.Add(-time.Hour)

	aged := vcr("aged", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &old, false)
	aged.Status.Data = &storagev1alpha1.VolumeDataBinding{
		Artifact: storagev1alpha1.VolumeDataArtifactRef{Kind: "PersistentVolume", Name: "orphan-pv"},
	}
	orphanPV := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "orphan-pv"}} // no ownerReferences

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		aged,
		vcr("young", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &fresh, false),
		orphanPV,
	).Build()
	m := &vcrGCManager{client: c, ttl: vcrTTL, now: func() time.Time { return vcrNow }}
	r := commongc.NewReconciler(c, nil, m)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, vcrReconcileReq("aged"))
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "aged"}, &storagev1alpha1.VolumeCaptureRequest{})),
		"aged terminal VCR must be collected")
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Name: "orphan-pv"}, &corev1.PersistentVolume{})),
		"the orphan artifact must be reaped by PreDelete during the reconcile (proves Reconcile → PreDelete → Delete)")

	_, err = r.Reconcile(ctx, vcrReconcileReq("young"))
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "young"}, &storagev1alpha1.VolumeCaptureRequest{}),
		"fresh terminal VCR must be retained")
}

func TestVRRGC_ReconcileThroughManager(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1alpha1.AddToScheme(scheme))

	old := vcrNow.Add(-25 * time.Hour)
	fresh := vcrNow.Add(-time.Hour)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		vrr("aged", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &old),
		vrr("young", readyC(metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted), &fresh),
	).Build()
	m := &vrrGCManager{client: c, ttl: vcrTTL, now: func() time.Time { return vcrNow }}
	r := commongc.NewReconciler(c, nil, m)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, vcrReconcileReq("aged"))
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "aged"}, &storagev1alpha1.VolumeRestoreRequest{})),
		"aged terminal VRR must be collected")

	_, err = r.Reconcile(ctx, vcrReconcileReq("young"))
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "young"}, &storagev1alpha1.VolumeRestoreRequest{}),
		"fresh terminal VRR must be retained")
}
