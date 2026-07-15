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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/deckhouse/pkg/log"
	"github.com/deckhouse/storage-foundation/hooks/go/consts"
)

// log.NewNop() returns a *log.Logger, which satisfies pkg.Logger — migrate takes pkg.Logger.
var testLogger = log.NewNop()

func TestMapLegacySpec_DataExport(t *testing.T) {
	cases := []struct {
		targetKind string
		wantGroup  string
		wantHas    bool
	}{
		{"PersistentVolumeClaim", "", false}, // core group -> no group field written
		{"VolumeSnapshot", "snapshot.storage.k8s.io", true},
		{"VirtualDisk", "virtualization.deckhouse.io", true},
		{"VirtualDiskSnapshot", "virtualization.deckhouse.io", true},
	}
	for _, tc := range cases {
		t.Run(tc.targetKind, func(t *testing.T) {
			spec := map[string]any{
				"targetRef": map[string]any{"kind": tc.targetKind, "name": "obj-1"},
			}
			got, err := mapLegacySpec("DataExport", spec)
			if err != nil {
				t.Fatalf("mapLegacySpec: unexpected error: %v", err)
			}
			group, has, _ := unstructured.NestedString(got, "targetRef", "group")
			if has != tc.wantHas {
				t.Errorf("group present = %v, want %v (group=%q)", has, tc.wantHas, group)
			}
			if group != tc.wantGroup {
				t.Errorf("targetRef.group = %q, want %q", group, tc.wantGroup)
			}
			// kind/name must be preserved.
			if k, _, _ := unstructured.NestedString(got, "targetRef", "kind"); k != tc.targetKind {
				t.Errorf("targetRef.kind = %q, want %q", k, tc.targetKind)
			}
			if n, _, _ := unstructured.NestedString(got, "targetRef", "name"); n != "obj-1" {
				t.Errorf("targetRef.name = %q, want %q", n, "obj-1")
			}
		})
	}
}

func TestMapLegacySpec_DataExportUnknownKind(t *testing.T) {
	spec := map[string]any{"targetRef": map[string]any{"kind": "Banana"}}
	if _, err := mapLegacySpec("DataExport", spec); err == nil {
		t.Fatal("expected error for unsupported targetRef.kind, got nil")
	}
}

func TestMapLegacySpec_DataImport(t *testing.T) {
	pvcTemplate := map[string]any{
		"spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}},
	}
	spec := map[string]any{
		"targetRef": map[string]any{"kind": "PersistentVolumeClaim", "pvcTemplate": pvcTemplate},
		"source":    map[string]any{"kind": "DataExport", "name": "exp-1"},
	}
	got, err := mapLegacySpec("DataImport", spec)
	if err != nil {
		t.Fatalf("mapLegacySpec: unexpected error: %v", err)
	}
	if _, found := got["targetRef"]; found {
		t.Error("targetRef must be dropped after DataImport migration")
	}
	if mode, _, _ := unstructured.NestedString(got, "mode"); mode != "CreatePVC" {
		t.Errorf("mode = %q, want CreatePVC", mode)
	}
	gotTmpl, found, _ := unstructured.NestedMap(got, "pvcTemplate")
	if !found {
		t.Fatal("pvcTemplate missing after migration")
	}
	if !reflect.DeepEqual(gotTmpl, pvcTemplate) {
		t.Errorf("pvcTemplate = %#v, want %#v", gotTmpl, pvcTemplate)
	}
	// Unrelated spec fields must survive.
	if _, found := got["source"]; !found {
		t.Error("unrelated spec field source was dropped")
	}
}

func TestMapLegacySpec_DataImportNoPVCTemplate(t *testing.T) {
	spec := map[string]any{"targetRef": map[string]any{"kind": "PersistentVolumeClaim"}}
	if _, err := mapLegacySpec("DataImport", spec); err == nil {
		t.Fatal("expected error for missing targetRef.pvcTemplate, got nil")
	}
}

func TestMapLegacySpec_UnexpectedKind(t *testing.T) {
	if _, err := mapLegacySpec("Banana", map[string]any{}); err == nil {
		t.Fatal("expected error for unexpected kind, got nil")
	}
}

func TestIsActiveLegacyResource(t *testing.T) {
	active := func(status map[string]any) bool {
		obj := &unstructured.Unstructured{Object: map[string]any{}}
		if status != nil {
			obj.Object["status"] = status
		}
		return isActiveLegacyResource(obj)
	}
	cond := func(conds ...map[string]any) map[string]any {
		s := make([]any, 0, len(conds))
		for _, c := range conds {
			s = append(s, c)
		}
		return map[string]any{"conditions": s}
	}

	if !active(nil) {
		t.Error("no status => should be active")
	}
	if !active(map[string]any{}) {
		t.Error("status without conditions => should be active")
	}
	if !active(cond(map[string]any{"type": "Ready", "status": "False", "reason": "InProgress"})) {
		t.Error("in-progress condition => should be active")
	}
	if active(cond(map[string]any{"type": "Completed", "status": string(metav1.ConditionTrue)})) {
		t.Error("Completed=True => should be inactive")
	}
	if active(cond(map[string]any{"type": "Expired", "status": string(metav1.ConditionTrue)})) {
		t.Error("Expired=True => should be inactive")
	}
	// Completed=False must NOT count as finished.
	if !active(cond(map[string]any{"type": "Completed", "status": string(metav1.ConditionFalse)})) {
		t.Error("Completed=False => should still be active")
	}
	for _, reason := range []string{"Expired", "ValidationFailed", "TargetNotFound", "TargetFailed", "PVConflict", "DeploymentFailed", "CleanupFailed"} {
		if active(cond(map[string]any{"type": "Ready", "status": "False", "reason": reason})) {
			t.Errorf("failed reason %q => should be inactive", reason)
		}
	}
}

func newFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = extv1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestStripLegacyPVCFinalizers(t *testing.T) {
	pvcBoth := pvc("d8-storage-foundation", "pvc-both", consts.LegacyStorageManagerFinalizerName, "kubernetes.io/pvc-protection")
	pvcLegacyOnly := pvc("d8-storage-foundation", "pvc-legacy", consts.LegacyStorageManagerFinalizerName)
	pvcOther := pvc("d8-storage-foundation", "pvc-other", "kubernetes.io/pvc-protection")

	cl := newFakeClient(pvcBoth, pvcLegacyOnly, pvcOther)
	if err := stripLegacyPVCFinalizers(context.Background(), cl, testLogger); err != nil {
		t.Fatalf("stripLegacyPVCFinalizers: %v", err)
	}

	assertPVCFinalizers(t, cl, "pvc-both", []string{"kubernetes.io/pvc-protection"})
	assertPVCFinalizers(t, cl, "pvc-legacy", nil)
	assertPVCFinalizers(t, cl, "pvc-other", []string{"kubernetes.io/pvc-protection"})
}

func TestMigrate(t *testing.T) {
	const ns = "d8-storage-foundation"

	activeExport := legacyCR("DataExport", ns, "exp-1", []string{consts.LegacyStorageManagerFinalizerName},
		map[string]any{"targetRef": map[string]any{"kind": "VolumeSnapshot", "name": "snap-1"}},
		nil,
	)
	finishedExport := legacyCR("DataExport", ns, "exp-done", []string{consts.LegacyStorageManagerFinalizerName},
		map[string]any{"targetRef": map[string]any{"kind": "VolumeSnapshot", "name": "snap-2"}},
		map[string]any{"conditions": []any{map[string]any{"type": "Completed", "status": "True"}}},
	)
	activeImport := legacyCR("DataImport", ns, "imp-1", []string{consts.LegacyStorageManagerFinalizerName},
		map[string]any{"targetRef": map[string]any{
			"kind":        "PersistentVolumeClaim",
			"pvcTemplate": map[string]any{"spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}}},
		}},
		nil,
	)
	legacyPVC := pvc(ns, "pvc-1", consts.LegacyStorageManagerFinalizerName, "kubernetes.io/pvc-protection")

	cl := newFakeClient(
		crd(consts.LegacyDataExportCRDName),
		crd(consts.LegacyDataImportCRDName),
		activeExport, finishedExport, activeImport, legacyPVC,
	)

	if err := migrate(context.Background(), cl, testLogger); err != nil {
		t.Fatalf("migrate: unexpected error: %v", err)
	}

	// Active export re-created under the unified group with the mapped targetRef.group.
	migratedExport := getUnified(t, cl, "DataExport", ns, "exp-1")
	if group, _, _ := unstructured.NestedString(migratedExport.Object, "spec", "targetRef", "group"); group != "snapshot.storage.k8s.io" {
		t.Errorf("migrated export targetRef.group = %q, want snapshot.storage.k8s.io", group)
	}

	// Active import re-created with mode=CreatePVC and no targetRef.
	migratedImport := getUnified(t, cl, "DataImport", ns, "imp-1")
	if mode, _, _ := unstructured.NestedString(migratedImport.Object, "spec", "mode"); mode != "CreatePVC" {
		t.Errorf("migrated import spec.mode = %q, want CreatePVC", mode)
	}
	if _, found, _ := unstructured.NestedMap(migratedImport.Object, "spec", "pvcTemplate"); !found {
		t.Error("migrated import missing spec.pvcTemplate")
	}
	if _, found, _ := unstructured.NestedMap(migratedImport.Object, "spec", "targetRef"); found {
		t.Error("migrated import must not carry spec.targetRef")
	}

	// Finished export must NOT be re-created.
	if err := getUnifiedErr(cl, "DataExport", ns, "exp-done"); !apierrors.IsNotFound(err) {
		t.Errorf("finished export should not be migrated, got err=%v", err)
	}

	// Legacy finalizers are stripped from ALL legacy CRs (active and finished alike).
	for _, name := range []string{"exp-1", "exp-done"} {
		old := getLegacy(t, cl, "DataExport", ns, name)
		if len(old.GetFinalizers()) != 0 {
			t.Errorf("legacy DataExport %s finalizers not stripped: %v", name, old.GetFinalizers())
		}
	}
	if old := getLegacy(t, cl, "DataImport", ns, "imp-1"); len(old.GetFinalizers()) != 0 {
		t.Errorf("legacy DataImport imp-1 finalizers not stripped: %v", old.GetFinalizers())
	}

	// Both legacy CRDs are deleted.
	for _, name := range []string{consts.LegacyDataExportCRDName, consts.LegacyDataImportCRDName} {
		crdObj := &extv1.CustomResourceDefinition{}
		if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, crdObj); !apierrors.IsNotFound(err) {
			t.Errorf("legacy CRD %s not deleted, got err=%v", name, err)
		}
	}

	// Legacy finalizer stripped from the PVC, other finalizers kept.
	assertPVCFinalizers(t, cl, "pvc-1", []string{"kubernetes.io/pvc-protection"})

	// Idempotent: a second converge with the legacy CRDs gone is a clean no-op.
	if err := migrate(context.Background(), cl, testLogger); err != nil {
		t.Fatalf("second migrate should be a no-op, got error: %v", err)
	}
}

// TestMigrate_ConcurrentDeletionTolerated simulates the svdm 025 hook racing this one: every
// finalizer-strip Patch 404s because the other hook (or its CRD-deletion cascade) already removed
// the object. migrate must treat that as success — not fail the converge — and still delete the
// legacy CRDs, so co-scheduled svdm+sf runs converge cleanly instead of ping-ponging retries.
func TestMigrate_ConcurrentDeletionTolerated(t *testing.T) {
	const ns = "d8-storage-foundation"

	activeImport := legacyCR("DataImport", ns, "imp-1", []string{consts.LegacyStorageManagerFinalizerName},
		map[string]any{"targetRef": map[string]any{
			"kind":        "PersistentVolumeClaim",
			"pvcTemplate": map[string]any{"spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}}},
		}},
		nil,
	)
	legacyPVC := pvc(ns, "pvc-1", consts.LegacyStorageManagerFinalizerName, "kubernetes.io/pvc-protection")

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = extv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(crd(consts.LegacyDataExportCRDName), crd(consts.LegacyDataImportCRDName), activeImport, legacyPVC).
		WithInterceptorFuncs(interceptor.Funcs{
			// Every finalizer strip goes through Patch; pretend the object vanished under us.
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return apierrors.NewNotFound(schema.GroupResource{Resource: obj.GetObjectKind().GroupVersionKind().Kind}, obj.GetName())
			},
		}).
		Build()

	if err := migrate(context.Background(), cl, testLogger); err != nil {
		t.Fatalf("migrate must tolerate NotFound on concurrent strip, got error: %v", err)
	}

	// The unified counterpart is still created (Create is not intercepted).
	getUnified(t, cl, "DataImport", ns, "imp-1")

	// And the legacy CRDs are still deleted (a NotFound strip must not block CRD cleanup).
	for _, name := range []string{consts.LegacyDataExportCRDName, consts.LegacyDataImportCRDName} {
		if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, &extv1.CustomResourceDefinition{}); !apierrors.IsNotFound(err) {
			t.Errorf("legacy CRD %s should be deleted despite NotFound strips, got err=%v", name, err)
		}
	}
}

// TestMigrate_PVCSweepFailureBlocksCRDDeletion locks the core data-safety invariant: a failed PVC
// finalizer sweep (a non-NotFound error) must block deletion of every legacy CRD, so a later
// converge retries the sweep instead of orphaning PVCs whose legacy finalizer was never removed.
func TestMigrate_PVCSweepFailureBlocksCRDDeletion(t *testing.T) {
	const ns = "d8-storage-foundation"
	legacyPVC := pvc(ns, "pvc-1", consts.LegacyStorageManagerFinalizerName)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = extv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(crd(consts.LegacyDataExportCRDName), crd(consts.LegacyDataImportCRDName), legacyPVC).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return apierrors.NewServiceUnavailable("simulated PVC patch failure")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	if err := migrate(context.Background(), cl, testLogger); err == nil {
		t.Fatal("migrate must return an error when the PVC sweep fails")
	}
	// Neither legacy CRD may be deleted while the sweep failed.
	for _, name := range []string{consts.LegacyDataExportCRDName, consts.LegacyDataImportCRDName} {
		if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, &extv1.CustomResourceDefinition{}); err != nil {
			t.Errorf("legacy CRD %s must NOT be deleted when the sweep failed, got err=%v", name, err)
		}
	}
}

// TestMigrate_UnmigratableActiveExportKeepsCRD verifies per-CRD gating: an active legacy DataExport
// that cannot be mapped (unsupported targetRef.kind) keeps its own CRD for a retry, but must not
// block deletion of the independent DataImport CRD.
func TestMigrate_UnmigratableActiveExportKeepsCRD(t *testing.T) {
	const ns = "d8-storage-foundation"
	badExport := legacyCR("DataExport", ns, "exp-bad", nil,
		map[string]any{"targetRef": map[string]any{"kind": "Banana", "name": "x"}}, nil)

	cl := newFakeClient(crd(consts.LegacyDataExportCRDName), crd(consts.LegacyDataImportCRDName), badExport)

	if err := migrate(context.Background(), cl, testLogger); err == nil {
		t.Fatal("migrate must return an error for an unsupported DataExport targetRef.kind")
	}
	// DataExport CRD kept (its migration failed) ...
	if err := cl.Get(context.Background(), client.ObjectKey{Name: consts.LegacyDataExportCRDName}, &extv1.CustomResourceDefinition{}); err != nil {
		t.Errorf("DataExport CRD must be kept when a resource under it fails to migrate, got err=%v", err)
	}
	// ... but the independent DataImport CRD is still cleaned up.
	if err := cl.Get(context.Background(), client.ObjectKey{Name: consts.LegacyDataImportCRDName}, &extv1.CustomResourceDefinition{}); !apierrors.IsNotFound(err) {
		t.Errorf("DataImport CRD should be deleted regardless of the DataExport failure, got err=%v", err)
	}
}

// TestMigrate_ConcurrentCRDDeletionOnListTolerated simulates the svdm 025 hook deleting a legacy CRD
// between migrate's up-front presence check and the per-kind List. The now-absent CRD must be
// treated as already-migrated (no error), not fail the whole converge.
func TestMigrate_ConcurrentCRDDeletionOnListTolerated(t *testing.T) {
	const ns = "d8-storage-foundation"
	activeImport := legacyCR("DataImport", ns, "imp-1", nil,
		map[string]any{"targetRef": map[string]any{
			"kind":        "PersistentVolumeClaim",
			"pvcTemplate": map[string]any{"spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}}},
		}}, nil)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = extv1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(crd(consts.LegacyDataExportCRDName), crd(consts.LegacyDataImportCRDName), activeImport).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Kind == "DataExportList" {
					// Concurrent svdm run removes the CRD right as we go to list it.
					_ = c.Delete(ctx, crd(consts.LegacyDataExportCRDName))
					return apierrors.NewNotFound(schema.GroupResource{Group: consts.LegacyAPIGroup, Resource: "dataexports"}, "")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

	if err := migrate(context.Background(), cl, testLogger); err != nil {
		t.Fatalf("migrate must treat a mid-flight legacy CRD deletion as a no-op, got: %v", err)
	}
	// The DataImport path still ran to completion.
	getUnified(t, cl, "DataImport", ns, "imp-1")
	for _, name := range []string{consts.LegacyDataExportCRDName, consts.LegacyDataImportCRDName} {
		if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, &extv1.CustomResourceDefinition{}); !apierrors.IsNotFound(err) {
			t.Errorf("legacy CRD %s should be gone, got err=%v", name, err)
		}
	}
}

// --- helpers ---

func crd(name string) *extv1.CustomResourceDefinition {
	return &extv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func pvc(ns, name string, finalizers ...string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Finalizers: finalizers},
	}
}

func legacyCR(kind, ns, name string, finalizers []string, spec, status map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: consts.LegacyAPIGroup, Version: "v1alpha1", Kind: kind})
	obj.SetNamespace(ns)
	obj.SetName(name)
	if len(finalizers) > 0 {
		obj.SetFinalizers(finalizers)
	}
	if spec != nil {
		_ = unstructured.SetNestedMap(obj.Object, spec, "spec")
	}
	if status != nil {
		_ = unstructured.SetNestedMap(obj.Object, status, "status")
	}
	return obj
}

func getUnifiedErr(cl client.Client, kind, ns, name string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: consts.APIGroup, Version: "v1alpha1", Kind: kind})
	return cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, obj)
}

func getUnified(t *testing.T, cl client.Client, kind, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: consts.APIGroup, Version: "v1alpha1", Kind: kind})
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatalf("get unified %s %s/%s: %v", kind, ns, name, err)
	}
	return obj
}

func getLegacy(t *testing.T, cl client.Client, kind, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: consts.LegacyAPIGroup, Version: "v1alpha1", Kind: kind})
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatalf("get legacy %s %s/%s: %v", kind, ns, name, err)
	}
	return obj
}

func assertPVCFinalizers(t *testing.T, cl client.Client, name string, want []string) {
	t.Helper()
	got := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "d8-storage-foundation", Name: name}, got); err != nil {
		t.Fatalf("get PVC %s: %v", name, err)
	}
	if !reflect.DeepEqual(got.Finalizers, want) {
		t.Errorf("PVC %s finalizers = %v, want %v", name, got.Finalizers, want)
	}
}
