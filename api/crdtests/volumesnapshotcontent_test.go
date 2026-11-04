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

package crdtests

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	handleOneOf    = "exactly one of volumeHandle and snapshotHandle must be set"
	snapshotRefXOR = "if spec.volumeSnapshotRef is set, both name and namespace must be specified together, or volumeSnapshotRef must be completely empty (VSC-only model)"
)

func volumeSnapshotContent(name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshotContent",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

// dynamicContentSpec is a content the CSI driver cuts from a volume for a
// bound VolumeSnapshot.
func dynamicContentSpec() map[string]any {
	return map[string]any{
		"deletionPolicy":   "Delete",
		"driver":           "csi.example.com",
		"sourceVolumeMode": "Filesystem",
		"source":           map[string]any{"volumeHandle": "vol-1"},
		"volumeSnapshotRef": map[string]any{
			"name":      "snap",
			"namespace": "default",
		},
	}
}

// preProvisionedContentSpec points at a snapshot that already exists in the
// storage.
func preProvisionedContentSpec() map[string]any {
	spec := dynamicContentSpec()
	spec["source"] = map[string]any{"snapshotHandle": "snap-1"}
	return spec
}

// vscOnlyContentSpec is a content with no VolumeSnapshot behind it.
func vscOnlyContentSpec() map[string]any {
	spec := dynamicContentSpec()
	delete(spec, "volumeSnapshotRef")
	return spec
}

func TestVolumeSnapshotContentCreate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		spec    func() map[string]any
		change  func(spec map[string]any)
		wantErr []string
	}{
		{name: "dynamic", spec: dynamicContentSpec},
		{name: "pre-provisioned", spec: preProvisionedContentSpec},
		{name: "VSC-only without volumeSnapshotRef", spec: vscOnlyContentSpec},
		{
			name:   "VSC-only with an empty volumeSnapshotRef",
			spec:   vscOnlyContentSpec,
			change: func(spec map[string]any) { spec["volumeSnapshotRef"] = map[string]any{} },
		},
		{
			name: "both handles",
			spec: dynamicContentSpec,
			change: func(spec map[string]any) {
				spec["source"] = map[string]any{"volumeHandle": "vol-1", "snapshotHandle": "snap-1"}
			},
			wantErr: []string{handleOneOf},
		},
		{
			name:    "no handle",
			spec:    dynamicContentSpec,
			change:  func(spec map[string]any) { spec["source"] = map[string]any{} },
			wantErr: []string{handleOneOf},
		},
		{
			name:    "volumeSnapshotRef without namespace",
			spec:    dynamicContentSpec,
			change:  func(spec map[string]any) { spec["volumeSnapshotRef"] = map[string]any{"name": "snap"} },
			wantErr: []string{snapshotRefXOR},
		},
		{
			name:    "volumeSnapshotRef without name",
			spec:    dynamicContentSpec,
			change:  func(spec map[string]any) { spec["volumeSnapshotRef"] = map[string]any{"namespace": "default"} },
			wantErr: []string{snapshotRefXOR},
		},
		{
			name: "volumeSnapshotRef with an empty name and namespace",
			spec: dynamicContentSpec,
			change: func(spec map[string]any) {
				spec["volumeSnapshotRef"] = map[string]any{"name": "", "namespace": ""}
			},
			wantErr: []string{snapshotRefXOR},
		},
		{
			name:    "unknown deletionPolicy",
			spec:    dynamicContentSpec,
			change:  func(spec map[string]any) { spec["deletionPolicy"] = "Recycle" },
			wantErr: []string{"spec.deletionPolicy", "Unsupported value"},
		},
		{
			name:    "without driver",
			spec:    dynamicContentSpec,
			change:  func(spec map[string]any) { delete(spec, "driver") },
			wantErr: []string{"spec.driver", "Required value"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := tt.spec()
			if tt.change != nil {
				tt.change(spec)
			}
			expectCreate(t, volumeSnapshotContent(objectName("create", tt.name), spec), tt.wantErr...)
		})
	}
}

func TestVolumeSnapshotContentUpdate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		spec    func() map[string]any
		change  func(spec map[string]any)
		wantErr []string
	}{
		{
			name:    "volumeHandle",
			spec:    dynamicContentSpec,
			change:  func(spec map[string]any) { spec["source"] = map[string]any{"volumeHandle": "vol-2"} },
			wantErr: []string{"volumeHandle is immutable"},
		},
		{
			name:    "volumeHandle replaced by snapshotHandle",
			spec:    dynamicContentSpec,
			change:  func(spec map[string]any) { spec["source"] = map[string]any{"snapshotHandle": "snap-1"} },
			wantErr: []string{"volumeHandle is required once set"},
		},
		{
			name:    "snapshotHandle",
			spec:    preProvisionedContentSpec,
			change:  func(spec map[string]any) { spec["source"] = map[string]any{"snapshotHandle": "snap-2"} },
			wantErr: []string{"snapshotHandle is immutable"},
		},
		{
			name:    "snapshotHandle replaced by volumeHandle",
			spec:    preProvisionedContentSpec,
			change:  func(spec map[string]any) { spec["source"] = map[string]any{"volumeHandle": "vol-1"} },
			wantErr: []string{"snapshotHandle is required once set"},
		},
		{
			name:    "sourceVolumeMode",
			spec:    dynamicContentSpec,
			change:  func(spec map[string]any) { spec["sourceVolumeMode"] = "Block" },
			wantErr: []string{"sourceVolumeMode is immutable"},
		},
		{
			name:    "sourceVolumeMode removed",
			spec:    dynamicContentSpec,
			change:  func(spec map[string]any) { delete(spec, "sourceVolumeMode") },
			wantErr: []string{"sourceVolumeMode is required once set"},
		},
		{
			name: "sourceVolumeMode set on a content created without it",
			spec: func() map[string]any {
				spec := dynamicContentSpec()
				delete(spec, "sourceVolumeMode")
				return spec
			},
			change: func(spec map[string]any) { spec["sourceVolumeMode"] = "Block" },
		},
		{
			name:   "deletionPolicy is mutable",
			spec:   dynamicContentSpec,
			change: func(spec map[string]any) { spec["deletionPolicy"] = "Retain" },
		},
		{
			name: "volumeSnapshotRef.uid is filled in on binding",
			spec: preProvisionedContentSpec,
			change: func(spec map[string]any) {
				spec["volumeSnapshotRef"] = map[string]any{"name": "snap", "namespace": "default", "uid": "snap-uid"}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			vsc := volumeSnapshotContent(objectName("update", tt.name), tt.spec())
			createObject(t, vsc)
			spec, _, err := unstructured.NestedMap(vsc.Object, "spec")
			if err != nil {
				t.Fatalf("read spec: %v", err)
			}
			tt.change(spec)
			vsc.Object["spec"] = spec
			expectAdmission(t, k8sClient.Update(context.Background(), vsc), tt.wantErr...)
		})
	}
}
