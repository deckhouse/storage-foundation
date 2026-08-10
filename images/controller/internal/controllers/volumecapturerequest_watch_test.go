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

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

func snapshotVCR(name string, uid types.UID, targetUID string) *storagev1alpha1.VolumeCaptureRequest {
	return &storagev1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: uid},
		Spec: storagev1alpha1.VolumeCaptureRequestSpec{
			Mode: ModeSnapshot,
			Target: &storagev1alpha1.VolumeCaptureTarget{
				UID:        targetUID,
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       "pvc",
			},
		},
	}
}

func newMapperClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1alpha1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&storagev1alpha1.VolumeCaptureRequest{}, vcrExpectedVSCNameIndex, indexVCRByExpectedVSCName).
		WithObjects(objects...).
		Build()
}

func TestIndexVCRByExpectedVSCName(t *testing.T) {
	t.Run("indexes a snapshot request by the content it will create", func(t *testing.T) {
		vcr := snapshotVCR("capture", "vcr-uid", "pvc-uid")

		assert.Equal(t, []string{snapshotVSCName("vcr-uid", "pvc-uid")}, indexVCRByExpectedVSCName(vcr))
	})

	t.Run("skips requests that cannot own a content", func(t *testing.T) {
		detach := snapshotVCR("detach", "vcr-uid", "pvc-uid")
		detach.Spec.Mode = ModeDetach
		assert.Nil(t, indexVCRByExpectedVSCName(detach))

		noTarget := snapshotVCR("no-target", "vcr-uid", "pvc-uid")
		noTarget.Spec.Target = nil
		assert.Nil(t, indexVCRByExpectedVSCName(noTarget))

		noUID := snapshotVCR("no-uid", "", "pvc-uid")
		assert.Nil(t, indexVCRByExpectedVSCName(noUID))

		noTargetUID := snapshotVCR("no-target-uid", "vcr-uid", "")
		assert.Nil(t, indexVCRByExpectedVSCName(noTargetUID))
	})

	t.Run("rejects objects of another kind", func(t *testing.T) {
		assert.Nil(t, indexVCRByExpectedVSCName(&snapshotv1.VolumeSnapshotContent{}))
	})
}

func TestMapVSCToVCR(t *testing.T) {
	owner := snapshotVCR("owner", "vcr-uid-owner", "pvc-uid-owner")
	other := snapshotVCR("other", "vcr-uid-other", "pvc-uid-other")
	ownedVSC := &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: snapshotVSCName(owner.UID, owner.Spec.Target.UID)},
	}
	ctx := context.Background()

	t.Run("routes a content event to its own request", func(t *testing.T) {
		r := &VolumeCaptureRequestController{Client: newMapperClient(t, owner, other)}

		got := r.mapVSCToVCR(ctx, ownedVSC)

		assert.Equal(t, []reconcile.Request{
			{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "owner"}},
		}, got)
	})

	t.Run("ignores a content nobody owns", func(t *testing.T) {
		r := &VolumeCaptureRequestController{Client: newMapperClient(t, owner, other)}
		foreign := &snapshotv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "snapcontent-created-by-someone-else"},
		}

		assert.Empty(t, r.mapVSCToVCR(ctx, foreign))
	})

	t.Run("a deleted request resolves to nothing", func(t *testing.T) {
		r := &VolumeCaptureRequestController{Client: newMapperClient(t, other)}

		assert.Empty(t, r.mapVSCToVCR(ctx, ownedVSC))
	})
}
