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

package handlers

import (
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestVolumeSnapshotProvenanceWireLiterals pins the key and the value this webhook writes. Both are read by
// another repository and by objects already living in a cluster, so a rename on this side is a silent
// contract break in both directions: snapshots created by this build would stop being recognised, and the
// reader would go on judging by a key nobody writes any more.
func TestVolumeSnapshotProvenanceWireLiterals(t *testing.T) {
	t.Parallel()

	if provenanceLabelName != "state-snapshotter.deckhouse.io/provenance" {
		t.Fatalf("unexpected provenance key: %s", provenanceLabelName)
	}
	if provenanceUserInCluster != "user-in-cluster" {
		t.Fatalf("unexpected provenance value: %s", provenanceUserInCluster)
	}
}

// ptr returns a pointer to v, for the bool fields of an owner reference.
func ptr[T any](v T) *T { return &v }

// volumeSnapshotWithMeta builds a VolumeSnapshot carrying the given labels and owner references, which is
// everything stateOriginOnCreate looks at.
func volumeSnapshotWithMeta(labels map[string]string, owners []metav1.OwnerReference) *snapshotv1.VolumeSnapshot {
	return &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snap",
			Namespace:       "test-ns",
			Labels:          labels,
			OwnerReferences: owners,
		},
	}
}

// TestStateOriginOnCreate enumerates every shape a CREATE payload can arrive in, because each row is a
// different party stating (or not stating) where the object came from:
//
//   - nobody stated anything: the ordinary case, a client creating its own snapshot, and the one this
//     handler exists for;
//   - the unified snapshot write-path already stated its own origin in the payload it is submitting: writing
//     over it would erase the record that this object is a node of a snapshot tree, and the consequence is
//     not cosmetic — the tree stops recognising its own node;
//   - some other value is already there: the same rule, stated in the form that does not depend on knowing
//     which values exist. This handler only ever ADDS a missing statement;
//   - the object arrives already controlled by another controller: it belongs to that controller's
//     lifecycle, and nothing of this stack's vocabulary may appear on it. A non-controller owner reference
//     is somebody's bookkeeping, not a lifecycle, so it does not withhold the statement.
func TestStateOriginOnCreate(t *testing.T) {
	t.Parallel()

	foreignController := metav1.OwnerReference{
		APIVersion: "virtualization.deckhouse.io/v1alpha2",
		Kind:       "VirtualDiskSnapshot",
		Name:       "vds",
		Controller: ptr(true),
	}
	plainReference := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "bookkeeping",
		Controller: ptr(false),
	}

	cases := []struct {
		name        string
		labels      map[string]string
		owners      []metav1.OwnerReference
		wantWritten bool   // whether the call reports having stated an origin
		wantPresent bool   // whether the provenance key exists afterwards
		wantValue   string // its value, when it exists
	}{
		{
			name:        "no labels at all",
			wantWritten: true,
			wantPresent: true,
			wantValue:   provenanceUserInCluster,
		},
		{
			name:        "unrelated labels only",
			labels:      map[string]string{"app": "web"},
			wantWritten: true,
			wantPresent: true,
			wantValue:   provenanceUserInCluster,
		},
		{
			name:        "the tree's own origin already in the payload",
			labels:      map[string]string{provenanceLabelName: "unified-snapshot"},
			wantPresent: true,
			wantValue:   "unified-snapshot",
		},
		{
			name:        "some other origin already in the payload",
			labels:      map[string]string{provenanceLabelName: "somebody-elses-write-path"},
			wantPresent: true,
			wantValue:   "somebody-elses-write-path",
		},
		{
			name:        "the key already there with an empty value",
			labels:      map[string]string{provenanceLabelName: ""},
			wantPresent: true,
			wantValue:   "",
		},
		{
			name:   "already controlled by a foreign controller",
			owners: []metav1.OwnerReference{foreignController},
		},
		{
			name:   "controlled by a foreign controller, listed after a plain reference",
			owners: []metav1.OwnerReference{plainReference, foreignController},
		},
		{
			name:        "a plain owner reference that is not the controller",
			owners:      []metav1.OwnerReference{plainReference},
			wantWritten: true,
			wantPresent: true,
			wantValue:   provenanceUserInCluster,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			snapshot := volumeSnapshotWithMeta(tc.labels, tc.owners)
			written := stateOriginOnCreate(snapshot)
			if written != tc.wantWritten {
				t.Fatalf("stateOriginOnCreate = %v, want %v (labels %#v)", written, tc.wantWritten, snapshot.Labels)
			}

			got, present := snapshot.Labels[provenanceLabelName]
			if present != tc.wantPresent {
				t.Fatalf("provenance key present = %v, want %v (labels %#v)", present, tc.wantPresent, snapshot.Labels)
			}
			if present && got != tc.wantValue {
				t.Fatalf("provenance = %q, want %q (labels %#v)", got, tc.wantValue, snapshot.Labels)
			}

			// Whatever the branch, the rest of the object's labels survive: this handler adds one key and
			// touches nothing else.
			for k, v := range tc.labels {
				if k == provenanceLabelName {
					continue
				}
				if snapshot.Labels[k] != v {
					t.Fatalf("label %q = %q, want %q preserved", k, snapshot.Labels[k], v)
				}
			}
		})
	}
	t.Logf("checked %d CREATE payload shapes", len(cases))
}

// TestStateOriginOnCreateIsIdempotent pins what a second pass must do, which is nothing. Admission is
// registered on CREATE only, so a second pass over the same object is not an ordinary event — but a webhook
// whose second call differs from its first is one whose result depends on how many times the API server
// happened to call it, and that is never the intended contract.
func TestStateOriginOnCreateIsIdempotent(t *testing.T) {
	t.Parallel()

	snapshot := volumeSnapshotWithMeta(nil, nil)
	if !stateOriginOnCreate(snapshot) {
		t.Fatal("the first pass must state the origin of an unclaimed snapshot")
	}
	first := snapshot.Labels[provenanceLabelName]

	if stateOriginOnCreate(snapshot) {
		t.Fatalf("the second pass must write nothing, labels=%#v", snapshot.Labels)
	}
	if got := snapshot.Labels[provenanceLabelName]; got != first {
		t.Fatalf("provenance changed on the second pass: %q -> %q", first, got)
	}
	if len(snapshot.Labels) != 1 {
		t.Fatalf("the second pass added labels: %#v", snapshot.Labels)
	}
}
