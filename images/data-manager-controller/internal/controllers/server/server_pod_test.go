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

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common/config"
)

const controllerNS = "d8-storage"

func podReconciler(t *testing.T, objs ...client.Object) *PodReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		if di, ok := o.(*dev1alpha1.DataImport); ok {
			b = b.WithStatusSubresource(di)
		}
	}
	c := b.WithObjects(objs...).Build()
	return &PodReconciler{Client: c, Reader: c, Config: &config.Options{ControllerNamespace: controllerNS}}
}

func req(ns, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func serverPod(name string, running bool, ownerNS, ownerName string) *corev1.Pod {
	phase := corev1.PodPending
	if running {
		phase = corev1.PodRunning
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: controllerNS,
			Labels:    map[string]string{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataImportValue},
			Annotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: ownerNS,
				dev1alpha1.AnnotationStorageManagerNameKey:      ownerName,
			},
		},
		Status: corev1.PodStatus{Phase: phase, PodIP: "10.0.0.9"},
	}
}

// TestPodReconcile_PodNotFound: a pod event for an already-deleted pod resolves cleanly (nil, no requeue).
func TestPodReconcile_PodNotFound(t *testing.T) {
	r := podReconciler(t)
	res, err := r.Reconcile(context.Background(), req(controllerNS, "gone-pod"))
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}

// TestPodReconcile_OwnerNotFound is the noise-plan regression: when the owning DataImport/DataExport is
// already gone (deleted or garbage-collected) while its pod is still terminating, the reconciler must
// return nil without requeue — not an error-requeue storm.
func TestPodReconcile_OwnerNotFound(t *testing.T) {
	pod := serverPod("orphan-pod", true, "app", "missing-di")
	r := podReconciler(t, pod)

	res, err := r.Reconcile(context.Background(), req(controllerNS, "orphan-pod"))
	require.NoError(t, err, "a running pod whose owner DataManager is gone must not error")
	assert.Equal(t, ctrl.Result{}, res)
}

// TestPodReconcile_UpdatesOwnerURL: the happy path still writes the pod URL onto the owner.
func TestPodReconcile_UpdatesOwnerURL(t *testing.T) {
	pod := serverPod("live-pod", true, "app", "di1")
	di := &dev1alpha1.DataImport{ObjectMeta: metav1.ObjectMeta{Name: "di1", Namespace: "app"}}
	r := podReconciler(t, pod, di)

	_, err := r.Reconcile(context.Background(), req(controllerNS, "live-pod"))
	require.NoError(t, err)

	got := &dev1alpha1.DataImport{}
	require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "di1"}, got))
	assert.Equal(t, "https://10.0.0.9:8085/", got.Status.Url)
}

// TestPodReconcile_OutsideControllerNamespace: pods outside the controller namespace are ignored.
func TestPodReconcile_OutsideControllerNamespace(t *testing.T) {
	r := podReconciler(t)
	res, err := r.Reconcile(context.Background(), req("some-other-ns", "whatever"))
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
}
