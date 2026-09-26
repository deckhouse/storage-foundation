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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common/config"
)

// importObjectMeta is the metadata the module stamps on every object it creates for DataImport owner.
func importObjectMeta(ns, name string, owner types.NamespacedName, finalizers ...string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Namespace:  ns,
		Name:       name,
		Labels:     map[string]string{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataImportValue},
		Finalizers: finalizers,
		Annotations: map[string]string{
			dev1alpha1.AnnotationStorageManagerNamespaceKey: owner.Namespace,
			dev1alpha1.AnnotationStorageManagerNameKey:      owner.Name,
		},
	}
}

// TestReconcile_ReleasesWhatAVanishedDataImportLeftBehind is the case found on an e2e cluster: a DataImport
// deleted inside its first reconcile, before its own finalizer was written, is gone at once, and its PVCs
// keep the module's finalizer forever — they stayed Terminating and blocked the cluster's destroy. The
// reconcile of the NotFound DataImport must release them, delete what is internal, keep what is the user's,
// and leave another DataImport's objects alone.
func TestReconcile_ReleasesWhatAVanishedDataImportLeftBehind(t *testing.T) {
	gone := types.NamespacedName{Namespace: "ns", Name: "import-immutable-mode"}
	other := types.NamespacedName{Namespace: "ns", Name: "still-running"}
	fin := dev1alpha1.StorageManagerFinalizerName

	objs := []client.Object{
		&appsv1.Deployment{ObjectMeta: importObjectMeta(teardownControllerNS, "deploy-for-pvc-gone", gone)},
		&corev1.PersistentVolumeClaim{ObjectMeta: importObjectMeta(teardownControllerNS, "import-pvc-for-pvc-gone", gone, fin)},
		&corev1.PersistentVolumeClaim{ObjectMeta: importObjectMeta("ns", "mode-lock-claim", gone, fin)},
		&appsv1.Deployment{ObjectMeta: importObjectMeta(teardownControllerNS, "deploy-for-pvc-other", other)},
		&corev1.PersistentVolumeClaim{ObjectMeta: importObjectMeta("ns", "other-claim", other, fin)},
	}
	c := fake.NewClientBuilder().WithScheme(teardownScheme(t)).WithObjects(objs...).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: teardownControllerNS}}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: gone})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	ctx := context.Background()
	err = c.Get(ctx, types.NamespacedName{Namespace: teardownControllerNS, Name: "deploy-for-pvc-gone"}, &appsv1.Deployment{})
	assert.True(t, apierrors.IsNotFound(err), "the importer Deployment of the vanished DataImport must be deleted, got %v", err)

	err = c.Get(ctx, types.NamespacedName{Namespace: teardownControllerNS, Name: "import-pvc-for-pvc-gone"}, &corev1.PersistentVolumeClaim{})
	assert.True(t, apierrors.IsNotFound(err), "the internal scratch PVC must be released and deleted, got %v", err)

	target := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "mode-lock-claim"}, target),
		"the user's target PVC must never be deleted")
	assert.False(t, controllerutil.ContainsFinalizer(target, fin), "the user's target PVC must lose the module's finalizer")
	assert.Nil(t, target.DeletionTimestamp, "releasing the user's PVC must not delete it")

	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: teardownControllerNS, Name: "deploy-for-pvc-other"}, &appsv1.Deployment{}),
		"another DataImport's importer must be left alone")
	otherClaim := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "other-claim"}, otherClaim))
	assert.True(t, controllerutil.ContainsFinalizer(otherClaim, fin), "another DataImport's PVC must keep its finalizer")
}

// TestReconcile_ReleasesATerminatingOrphanPVC is the exact state found on the cluster: the PVC was already
// being deleted and stayed Terminating because of the finalizer. Removing it is what lets the deletion finish.
func TestReconcile_ReleasesATerminatingOrphanPVC(t *testing.T) {
	gone := types.NamespacedName{Namespace: "ns", Name: "import-immutable-mode"}
	fin := dev1alpha1.StorageManagerFinalizerName
	now := metav1.Now()

	meta := importObjectMeta("ns", "mode-lock-claim", gone, fin)
	meta.DeletionTimestamp = &now
	c := fake.NewClientBuilder().WithScheme(teardownScheme(t)).WithObjects(&corev1.PersistentVolumeClaim{ObjectMeta: meta}).Build()
	r := &DataImportReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: teardownControllerNS}}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: gone})
	require.NoError(t, err)

	err = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "mode-lock-claim"}, &corev1.PersistentVolumeClaim{})
	assert.True(t, apierrors.IsNotFound(err), "with the finalizer gone, the pending deletion must complete, got %v", err)
}
