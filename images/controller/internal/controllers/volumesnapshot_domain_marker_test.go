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

package controllers

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ssstoragev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	sfsnapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
)

// newDomainMarkerReconciler builds a VolumeSnapshotDomainReconciler whose SnapClient is a fake client
// carrying the given extended VolumeSnapshot, using the extended scheme the real reconciler relies on.
func newDomainMarkerReconciler(t *testing.T, vs *sfsnapv1.VolumeSnapshot) *VolumeSnapshotDomainReconciler {
	t.Helper()
	scheme := apiruntime.NewScheme()
	if err := sfsnapv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add sfsnapv1: %v", err)
	}
	snapClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vs).Build()
	return &VolumeSnapshotDomainReconciler{SnapClient: snapClient}
}

// TestLatchManagedTrueStampsDeleteProtected asserts a VolumeSnapshot adopted as a managed tree node
// (managed=true) is stamped with the authoritative delete-protection marker in the SAME latch patch,
// i.e. before any graph edge is published (delete-protection-contract.md §6.1/§8.1).
func TestLatchManagedTrueStampsDeleteProtected(t *testing.T) {
	ctx := context.Background()
	vs := &sfsnapv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "vs-managed", Namespace: "ns"}}
	r := newDomainMarkerReconciler(t, vs)

	got, err := r.latchManagedLabel(ctx, client.ObjectKeyFromObject(vs), managedValueTrue)
	if err != nil {
		t.Fatalf("latchManagedLabel: %v", err)
	}
	if got != managedValueTrue {
		t.Fatalf("effective managed = %q, want %q", got, managedValueTrue)
	}

	cur := &sfsnapv1.VolumeSnapshot{}
	if err := r.SnapClient.Get(ctx, client.ObjectKeyFromObject(vs), cur); err != nil {
		t.Fatalf("get vs: %v", err)
	}
	if cur.Labels[labelSnapshotManaged] != managedValueTrue {
		t.Fatalf("managed label = %q, want %q", cur.Labels[labelSnapshotManaged], managedValueTrue)
	}
	if !ssstoragev1alpha1.IsDeleteProtected(cur) {
		t.Fatalf("managed VS must be delete-protected, labels=%#v", cur.Labels)
	}
}

// TestLatchManagedFalseDoesNotStampDeleteProtected asserts a vetoed (managed=false) VolumeSnapshot is a
// plain CSI snapshot, NOT a tree node, and therefore MUST NOT be delete-protected (§8.1/§8.5).
func TestLatchManagedFalseDoesNotStampDeleteProtected(t *testing.T) {
	ctx := context.Background()
	vs := &sfsnapv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "vs-vetoed", Namespace: "ns"}}
	r := newDomainMarkerReconciler(t, vs)

	if _, err := r.latchManagedLabel(ctx, client.ObjectKeyFromObject(vs), managedValueFalse); err != nil {
		t.Fatalf("latchManagedLabel: %v", err)
	}

	cur := &sfsnapv1.VolumeSnapshot{}
	if err := r.SnapClient.Get(ctx, client.ObjectKeyFromObject(vs), cur); err != nil {
		t.Fatalf("get vs: %v", err)
	}
	if cur.Labels[labelSnapshotManaged] != managedValueFalse {
		t.Fatalf("managed label = %q, want %q", cur.Labels[labelSnapshotManaged], managedValueFalse)
	}
	if ssstoragev1alpha1.IsDeleteProtected(cur) {
		t.Fatalf("vetoed VS must NOT be delete-protected, labels=%#v", cur.Labels)
	}
}
