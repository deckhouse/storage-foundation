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

package hooks_common

import (
	"context"
	"testing"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/deckhouse/pkg/log"
	"github.com/deckhouse/storage-foundation/hooks/go/consts"
)

// log.NewNop() returns a *log.Logger, which satisfies pkg.Logger — cleanup takes pkg.Logger.
var testLogger = log.NewNop()

// TestCleanup_StripsFinalizersFromUserNamespaceCRs locks the cluster-wide listing contract: the
// namespaced kinds in consts.CRGVKsForFinalizerRemoval (VolumeSnapshot, DataExport, DataImport)
// live in arbitrary USER namespaces, so the on-delete cleanup must find them there — a list
// confined to the module namespace would leave their finalizers behind and hang deletion once the
// module's controller is gone.
func TestCleanup_StripsFinalizersFromUserNamespaceCRs(t *testing.T) {
	const userNS = "user-app-1"

	snapshot := &snapv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  userNS,
			Name:       "snap-1",
			Finalizers: []string{"snapshot.storage.kubernetes.io/volumesnapshot-bound-protection"},
		},
	}
	content := &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "content-1",
			Finalizers: []string{"snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"},
		},
	}
	dataExport := &unstructured.Unstructured{}
	dataExport.SetGroupVersionKind(schema.GroupVersionKind{Group: consts.APIGroup, Version: "v1alpha1", Kind: "DataExport"})
	dataExport.SetNamespace(userNS)
	dataExport.SetName("exp-1")
	dataExport.SetFinalizers([]string{consts.APIGroup + "/storage-foundation-controller"})

	// A module-namespace Secret with a finalizer — the pre-existing path must keep working.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  consts.ModuleNamespace,
			Name:       "webhook-certs",
			Finalizers: []string{"storage-foundation.deckhouse.io/keep"},
		},
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = snapv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(snapshot, content, dataExport, secret).
		Build()

	if err := cleanup(context.Background(), cl, testLogger); err != nil {
		t.Fatalf("cleanup: unexpected error: %v", err)
	}

	gotSnap := &snapv1.VolumeSnapshot{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: userNS, Name: "snap-1"}, gotSnap); err != nil {
		t.Fatalf("get VolumeSnapshot: %v", err)
	}
	if len(gotSnap.Finalizers) != 0 {
		t.Errorf("user-namespace VolumeSnapshot finalizers not stripped: %v", gotSnap.Finalizers)
	}

	gotContent := &snapv1.VolumeSnapshotContent{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "content-1"}, gotContent); err != nil {
		t.Fatalf("get VolumeSnapshotContent: %v", err)
	}
	if len(gotContent.Finalizers) != 0 {
		t.Errorf("cluster-scoped VolumeSnapshotContent finalizers not stripped: %v", gotContent.Finalizers)
	}

	gotExport := &unstructured.Unstructured{}
	gotExport.SetGroupVersionKind(schema.GroupVersionKind{Group: consts.APIGroup, Version: "v1alpha1", Kind: "DataExport"})
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: userNS, Name: "exp-1"}, gotExport); err != nil {
		t.Fatalf("get DataExport: %v", err)
	}
	if len(gotExport.GetFinalizers()) != 0 {
		t.Errorf("user-namespace DataExport finalizers not stripped: %v", gotExport.GetFinalizers())
	}

	gotSecret := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: consts.ModuleNamespace, Name: "webhook-certs"}, gotSecret); err != nil {
		t.Fatalf("get Secret: %v", err)
	}
	if len(gotSecret.Finalizers) != 0 {
		t.Errorf("module-namespace Secret finalizers not stripped: %v", gotSecret.Finalizers)
	}
}
