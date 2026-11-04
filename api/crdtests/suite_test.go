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

// Package crdtests checks what kube-apiserver admits and rejects once the
// CRDs in crds/ and crds/internal are installed. It is a module of its own so
// that envtest, the controller-runtime client and sds-common-lib stay out of
// the requirements of the api module, which other modules import.
package crdtests

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/deckhouse/sds-common-lib/crdtest"
	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

var k8sClient client.Client

// TestMain installs the module's own CRDs. envtest reads only the files
// directly in a directory, so crds/internal is listed on its own; crds/vendor
// holds a third-party CRD these tests do not cover.
func TestMain(m *testing.M) {
	os.Exit(crdtest.Main(m, crdtest.Options{
		CRDPaths:    []string{"../../crds", "../../crds/internal"},
		AddToScheme: []func(*runtime.Scheme) error{v1alpha1.AddToScheme},
	}, func(env *crdtest.Env) { k8sClient = env.Client }))
}

// objectName turns a subtest name into an object name the apiserver accepts.
func objectName(prefix, name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(prefix + "-" + name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if !strings.HasSuffix(b.String(), "-") {
			b.WriteRune('-')
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// createNamespace creates a namespace for the namespaced objects of one test.
// envtest runs no namespace controller, so a deleted namespace never goes away
// and every test needs a name of its own.
func createNamespace(t *testing.T, name string) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create Namespace %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ns) })
	return name
}

// createObject creates obj and deletes it when the test ends.
func createObject(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8sClient.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
}

// expectCreate creates obj, deletes it when the test ends if it was admitted,
// and checks the outcome with expectAdmission.
func expectCreate(t *testing.T, obj client.Object, messages ...string) {
	t.Helper()
	err := k8sClient.Create(context.Background(), obj)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
	}
	expectAdmission(t, err, messages...)
}

// toUnstructured converts a typed object into the map the client sends, so a
// test can drop a field or set it to the empty string where omitempty or a
// non-pointer struct keeps the typed client from doing so.
func toUnstructured(obj client.Object) *unstructured.Unstructured {
	gvk, err := apiutil.GVKForObject(obj, k8sClient.Scheme())
	if err != nil {
		panic(fmt.Sprintf("GVK of %T: %v", obj, err))
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		panic(fmt.Sprintf("convert %T: %v", obj, err))
	}
	u := &unstructured.Unstructured{Object: content}
	u.SetGroupVersionKind(gvk)
	return u
}

// setField sets a nested field of u, the path given as separate keys.
func setField(u *unstructured.Unstructured, value any, path ...string) {
	if err := unstructured.SetNestedField(u.Object, value, path...); err != nil {
		panic(fmt.Sprintf("set %s: %v", strings.Join(path, "."), err))
	}
}

// expectAdmission checks that err is nil when no messages are given, and
// otherwise that the apiserver rejected the object as invalid with every one of
// messages in the error.
func expectAdmission(t *testing.T, err error, messages ...string) {
	t.Helper()
	if len(messages) == 0 {
		if err != nil {
			t.Fatalf("want the object admitted, got %v", err)
		}
		return
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("want an Invalid error with %q, got %v", messages, err)
	}
	for _, message := range messages {
		if !strings.Contains(err.Error(), message) {
			t.Fatalf("want %q in the error, got %v", message, err)
		}
	}
}
