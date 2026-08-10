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
	"strings"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// makeStorageClass builds a StorageClass that is managed by the platform or not, with or without the
// VolumeSnapshotClass annotation.
func makeStorageClass(name string, managed bool, snapshotClass string) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if managed {
		sc.Labels = map[string]string{storageClassManagedbyLabelName: "sds-replicated-volume"}
	}
	if snapshotClass != "" {
		sc.Annotations = map[string]string{storageClassVolumeSnapshotAnnotationName: snapshotClass}
	}
	return sc
}

func ptr(s string) *string { return &s }

// TestResolveVolumeSnapshotClass_ManagedWithoutAnnotation covers the case the whole webhook fix exists
// for: a managed StorageClass with no snapshot class annotation is a misconfiguration, and admitting the
// snapshot only defers the failure to a point where nobody can tell what went wrong. The refusal has to
// name the StorageClass and the annotation, or the user is left with a generic webhook denial.
func TestResolveVolumeSnapshotClass_ManagedWithoutAnnotation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested *string
	}{
		{name: "no class requested is still denied", requested: nil},
		{name: "an explicitly requested class does not excuse the missing annotation", requested: ptr("some-class")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, mutate, err := resolveVolumeSnapshotClass(makeStorageClass("ceph-rbd-sc", true, ""), tt.requested)
			if err == nil {
				t.Fatalf("a managed StorageClass without %s must be denied", storageClassVolumeSnapshotAnnotationName)
			}
			if mutate {
				t.Fatalf("a denied snapshot must not also be mutated")
			}
			for _, want := range []string{"ceph-rbd-sc", storageClassVolumeSnapshotAnnotationName} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q must name %q", err.Error(), want)
				}
			}
		})
	}
}

// TestResolveVolumeSnapshotClass_ManagedWithAnnotation: with the annotation present the class is filled
// in, and a conflicting explicit request is refused instead of being silently overwritten.
func TestResolveVolumeSnapshotClass_ManagedWithAnnotation(t *testing.T) {
	t.Parallel()

	sc := makeStorageClass("ceph-rbd-sc", true, "ceph-rbd-vsc")

	tests := []struct {
		name       string
		requested  *string
		wantClass  string
		wantMutate bool
		wantDenied []string
	}{
		{
			name:       "an unset class is filled from the annotation",
			requested:  nil,
			wantClass:  "ceph-rbd-vsc",
			wantMutate: true,
		},
		{
			name:       "a matching class is accepted",
			requested:  ptr("ceph-rbd-vsc"),
			wantClass:  "ceph-rbd-vsc",
			wantMutate: true,
		},
		{
			name:       "a conflicting class is denied and names both sides",
			requested:  ptr("someone-elses-vsc"),
			wantDenied: []string{"someone-elses-vsc", "ceph-rbd-vsc", "ceph-rbd-sc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			class, mutate, err := resolveVolumeSnapshotClass(sc, tt.requested)
			if len(tt.wantDenied) > 0 {
				if err == nil {
					t.Fatalf("a class conflicting with the annotation must be denied")
				}
				for _, want := range tt.wantDenied {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("message %q must name %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected denial: %v", err)
			}
			if mutate != tt.wantMutate || class != tt.wantClass {
				t.Fatalf("got class %q mutate %v, want %q %v", class, mutate, tt.wantClass, tt.wantMutate)
			}
		})
	}
}

// TestResolveVolumeSnapshotClass_Unmanaged pins the behavior for foreign StorageClasses: the fix must
// not start denying snapshots on storage this module does not own.
func TestResolveVolumeSnapshotClass_Unmanaged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sc         *storagev1.StorageClass
		requested  *string
		wantClass  string
		wantMutate bool
	}{
		{
			name: "no annotation and no request is left alone",
			sc:   makeStorageClass("foreign-sc", false, ""),
		},
		{
			name:      "an explicit request is never overridden",
			sc:        makeStorageClass("foreign-sc", false, "hinted-vsc"),
			requested: ptr("user-chosen-vsc"),
		},
		{
			name:       "an annotation is still used as a hint when nothing was requested",
			sc:         makeStorageClass("foreign-sc", false, "hinted-vsc"),
			wantClass:  "hinted-vsc",
			wantMutate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			class, mutate, err := resolveVolumeSnapshotClass(tt.sc, tt.requested)
			if err != nil {
				t.Fatalf("an unmanaged StorageClass must never be denied, got %v", err)
			}
			if mutate != tt.wantMutate || class != tt.wantClass {
				t.Fatalf("got class %q mutate %v, want %q %v", class, mutate, tt.wantClass, tt.wantMutate)
			}
		})
	}
}

// TestManagedByLabelMatchesReality guards the literal itself: the earlier module-specific name matched no
// StorageClass in any cluster, so the managed branch never ran and the denials above were unreachable.
func TestManagedByLabelMatchesReality(t *testing.T) {
	t.Parallel()

	if storageClassManagedbyLabelName != "storage.deckhouse.io/managed-by" {
		t.Fatalf("managed-by label = %q; producers set storage.deckhouse.io/managed-by, and any change here must move with them",
			storageClassManagedbyLabelName)
	}
}
