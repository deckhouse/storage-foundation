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

// TestLatchNeverStampsDeleteProtected asserts the latch writes its own outcome and nothing else, on BOTH
// outcomes. Protection says the object is a node of a snapshot tree, which its creator knows and an
// observer does not: a reconciler that stamps it while latching turns "I have seen this object" into "I own
// this object", and that is how a foreign snapshot became undeletable. A snapshot this reconciler may
// legitimately manage is already protected when it first observes it, so nothing is lost by removing the stamp.
func TestLatchNeverStampsDeleteProtected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired string
	}{
		{name: "managed", desired: managedValueTrue},
		{name: "vetoed", desired: managedValueFalse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			vs := &sfsnapv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "vs-" + tc.name, Namespace: "ns"}}
			r := newDomainMarkerReconciler(t, vs)

			got, err := r.latchManagedLabel(ctx, client.ObjectKeyFromObject(vs), tc.desired)
			if err != nil {
				t.Fatalf("latchManagedLabel: %v", err)
			}
			if got != tc.desired {
				t.Fatalf("effective managed = %q, want %q", got, tc.desired)
			}

			cur := &sfsnapv1.VolumeSnapshot{}
			if err := r.SnapClient.Get(ctx, client.ObjectKeyFromObject(vs), cur); err != nil {
				t.Fatalf("get vs: %v", err)
			}
			if cur.Labels[labelSnapshotManaged] != tc.desired {
				t.Fatalf("managed label = %q, want %q", cur.Labels[labelSnapshotManaged], tc.desired)
			}
			if ssstoragev1alpha1.IsDeleteProtected(cur) {
				t.Fatalf("the latch must not write protection, labels=%#v", cur.Labels)
			}
		})
	}
}

// TestHasForeignControllerOwner enumerates the owner shapes the domain gate has to tell apart. Only one row
// is vetoed — a controller from another API group — and the interesting rows are the ones that are not: a
// snapshot with no owner is a user's own snapshot and gets domain capture like any other, and a foreign
// reference that is not the controller belongs to somebody's bookkeeping, not to a lifecycle.
func TestHasForeignControllerOwner(t *testing.T) {
	controller := true
	notController := false
	ourAPIVersion := ssstoragev1alpha1.APIGroup + "/" + ssstoragev1alpha1.APIVersion

	cases := []struct {
		name  string
		owner []metav1.OwnerReference
		want  bool
	}{
		{
			name:  "a foreign controller",
			owner: []metav1.OwnerReference{{APIVersion: "virtualization.deckhouse.io/v1alpha2", Kind: "VirtualDiskSnapshot", Name: "vds", Controller: &controller}},
			want:  true,
		},
		{
			name:  "an unparsable owner apiVersion",
			owner: []metav1.OwnerReference{{APIVersion: "a/b/c", Kind: "Whatever", Name: "x", Controller: &controller}},
			want:  true,
		},
		{
			name:  "our Snapshot as controller",
			owner: []metav1.OwnerReference{{APIVersion: ourAPIVersion, Kind: ssstoragev1alpha1.SnapshotKind, Name: "root", UID: "root-uid", Controller: &controller}},
			want:  false,
		},
		{
			name:  "our Snapshot at a future version",
			owner: []metav1.OwnerReference{{APIVersion: ssstoragev1alpha1.APIGroup + "/v1", Kind: ssstoragev1alpha1.SnapshotKind, Name: "root", Controller: &controller}},
			want:  false,
		},
		{
			name:  "no owner at all",
			owner: nil,
			want:  false,
		},
		{
			name:  "a foreign owner that is not the controller",
			owner: []metav1.OwnerReference{{APIVersion: "virtualization.deckhouse.io/v1alpha2", Kind: "VirtualDiskSnapshot", Name: "vds", Controller: &notController}},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs := &sfsnapv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "vs", Namespace: "ns", OwnerReferences: tc.owner}}
			if got := hasForeignControllerOwner(vs); got != tc.want {
				t.Fatalf("hasForeignControllerOwner(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
