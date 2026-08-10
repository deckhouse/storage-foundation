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
	"io"
	"log/slog"
	"strings"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func makeVolumeSnapshot(requestedClass *string) *snapshotv1.VolumeSnapshot {
	return &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "default"},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source:                  snapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: ptr("pvc")},
			VolumeSnapshotClassName: requestedClass,
		},
	}
}

// TestVolumeSnapshotClassVerdict_Denies is the point of the validating webhook: a misconfiguration must
// come back as a denial carrying the actionable message, which is what the user actually reads. The same
// verdict raised from the mutator reaches the user as "Internal error occurred" with the message buried in
// an escaped JSON body, i.e. as a cluster failure rather than something they can fix.
func TestVolumeSnapshotClassVerdict_Denies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		sc        *storagev1.StorageClass
		requested *string
		wantNamed []string
	}{
		{
			name:      "managed StorageClass without the snapshot class annotation",
			sc:        makeStorageClass("ceph-rbd-sc", true, ""),
			wantNamed: []string{"ceph-rbd-sc", storageClassVolumeSnapshotAnnotationName},
		},
		{
			name:      "requested class conflicting with the annotation",
			sc:        makeStorageClass("ceph-rbd-sc", true, "ceph-rbd-vsc"),
			requested: ptr("someone-elses-vsc"),
			wantNamed: []string{"someone-elses-vsc", "ceph-rbd-vsc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := volumeSnapshotClassVerdict(discardLogger(), tt.sc, makeVolumeSnapshot(tt.requested))
			if res.Valid {
				t.Fatalf("the request must be denied, not admitted")
			}
			for _, want := range tt.wantNamed {
				if !strings.Contains(res.Message, want) {
					t.Errorf("denial message %q must name %q", res.Message, want)
				}
			}
		})
	}
}

// TestVolumeSnapshotClassVerdict_Admits: the validator only refuses what resolveVolumeSnapshotClass
// refuses. Everything the mutator can handle — including storage this module does not own — must pass.
func TestVolumeSnapshotClassVerdict_Admits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		sc        *storagev1.StorageClass
		requested *string
	}{
		{name: "managed and annotated, class filled by the mutator", sc: makeStorageClass("ceph-rbd-sc", true, "ceph-rbd-vsc")},
		{name: "managed and annotated, matching class requested", sc: makeStorageClass("ceph-rbd-sc", true, "ceph-rbd-vsc"), requested: ptr("ceph-rbd-vsc")},
		{name: "unmanaged StorageClass without annotation", sc: makeStorageClass("foreign-sc", false, "")},
		{name: "unmanaged StorageClass with an explicit request", sc: makeStorageClass("foreign-sc", false, "hinted-vsc"), requested: ptr("user-chosen-vsc")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := volumeSnapshotClassVerdict(discardLogger(), tt.sc, makeVolumeSnapshot(tt.requested))
			if !res.Valid {
				t.Fatalf("unexpected denial: %s", res.Message)
			}
		})
	}
}

// TestMutationDefersRefusalToValidation pins the split that keeps the denial readable: on a configuration
// verdict the mutator neither errors (which kubewebhook answers with HTTP 500) nor invents a class — it
// leaves the object untouched and lets validation, which runs after mutation, refuse it.
func TestMutationDefersRefusalToValidation(t *testing.T) {
	t.Parallel()

	snapshot := makeVolumeSnapshot(nil)

	res := mutationForStorageClass(discardLogger(), makeStorageClass("ceph-rbd-sc", true, ""), snapshot)

	if res.MutatedObject != nil {
		t.Fatalf("a refused snapshot must not be mutated, got %#v", res.MutatedObject)
	}
	if snapshot.Spec.VolumeSnapshotClassName != nil {
		t.Fatalf("spec.volumeSnapshotClassName must stay unset, got %q", *snapshot.Spec.VolumeSnapshotClassName)
	}
}
