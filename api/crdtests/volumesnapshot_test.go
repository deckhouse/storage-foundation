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

// The snapshot.storage.k8s.io CRDs are maintained by hand in crds/ and carry
// fields no Go type in this repository models, so their objects are built as
// maps.

const (
	sourceRequired      = "source is required unless mode is Import"
	importWithSource    = "an import-mode VolumeSnapshot (mode = Import) must have no CSI source"
	sourceOneOf         = "source must be empty (restore intent / import mode) or exactly one of persistentVolumeClaimName and volumeSnapshotContentName must be set"
	snapshotModeChanged = "mode is immutable"
)

func volumeSnapshot(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"spec":       spec,
	}}
}

// captureSpec is a dynamic snapshot of a PersistentVolumeClaim.
func captureSpec() map[string]any {
	return map[string]any{
		"mode":                    "Capture",
		"volumeSnapshotClassName": "csi",
		"source":                  map[string]any{"persistentVolumeClaimName": "data"},
	}
}

// preProvisionedSpec binds an existing VolumeSnapshotContent.
func preProvisionedSpec() map[string]any {
	return map[string]any{
		"mode":   "Capture",
		"source": map[string]any{"volumeSnapshotContentName": "snapcontent"},
	}
}

// importSpec is a VolumeSnapshot whose data comes from a DataImport.
func importSpec() map[string]any {
	return map[string]any{"mode": "Import"}
}

// restoreIntentSpec carries an empty source that a VolumeSnapshotContent name
// is set on later.
func restoreIntentSpec() map[string]any {
	return map[string]any{"mode": "Capture", "source": map[string]any{}}
}

func TestVolumeSnapshotCreate(t *testing.T) {
	ns := createNamespace(t, "volumesnapshot-create")

	for _, tt := range []struct {
		name    string
		spec    func() map[string]any
		change  func(spec map[string]any)
		wantErr []string
	}{
		{name: "Capture from a PersistentVolumeClaim", spec: captureSpec},
		{name: "Capture from a VolumeSnapshotContent", spec: preProvisionedSpec},
		{name: "restore intent with an empty source", spec: restoreIntentSpec},
		{name: "Import without source", spec: importSpec},
		{
			name:   "Import with an empty source",
			spec:   importSpec,
			change: func(spec map[string]any) { spec["source"] = map[string]any{} },
		},
		{
			name:   "without mode",
			spec:   captureSpec,
			change: func(spec map[string]any) { delete(spec, "mode") },
		},
		{
			name:    "Capture without source",
			spec:    captureSpec,
			change:  func(spec map[string]any) { delete(spec, "source") },
			wantErr: []string{sourceRequired},
		},
		{
			name: "without mode and source",
			spec: captureSpec,
			change: func(spec map[string]any) {
				delete(spec, "mode")
				delete(spec, "source")
			},
			wantErr: []string{sourceRequired},
		},
		{
			name: "Import from a PersistentVolumeClaim",
			spec: importSpec,
			change: func(spec map[string]any) {
				spec["source"] = map[string]any{"persistentVolumeClaimName": "data"}
			},
			wantErr: []string{importWithSource},
		},
		{
			name: "Import from a VolumeSnapshotContent",
			spec: importSpec,
			change: func(spec map[string]any) {
				spec["source"] = map[string]any{"volumeSnapshotContentName": "snapcontent"}
			},
			wantErr: []string{importWithSource},
		},
		{
			name: "both sources",
			spec: captureSpec,
			change: func(spec map[string]any) {
				spec["source"] = map[string]any{
					"persistentVolumeClaimName": "data",
					"volumeSnapshotContentName": "snapcontent",
				}
			},
			wantErr: []string{sourceOneOf},
		},
		{
			name:    "empty volumeSnapshotClassName",
			spec:    captureSpec,
			change:  func(spec map[string]any) { spec["volumeSnapshotClassName"] = "" },
			wantErr: []string{"volumeSnapshotClassName must not be the empty string when set"},
		},
		{
			name:    "unknown mode",
			spec:    captureSpec,
			change:  func(spec map[string]any) { spec["mode"] = "Clone" },
			wantErr: []string{"spec.mode", "Unsupported value"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := tt.spec()
			if tt.change != nil {
				tt.change(spec)
			}
			expectCreate(t, volumeSnapshot(ns, objectName("create", tt.name), spec), tt.wantErr...)
		})
	}

	t.Run("without spec", func(t *testing.T) {
		vs := volumeSnapshot(ns, "create-without-spec", nil)
		delete(vs.Object, "spec")
		expectCreate(t, vs, "spec: Required value")
	})
}

func TestVolumeSnapshotUpdate(t *testing.T) {
	ns := createNamespace(t, "volumesnapshot-update")

	for _, tt := range []struct {
		name    string
		spec    func() map[string]any
		change  func(spec map[string]any)
		wantErr []string
	}{
		{
			name: "mode",
			spec: captureSpec,
			change: func(spec map[string]any) {
				spec["mode"] = "Import"
				delete(spec, "source")
			},
			wantErr: []string{"spec.mode", snapshotModeChanged},
		},
		{
			name:    "mode removed",
			spec:    captureSpec,
			change:  func(spec map[string]any) { delete(spec, "mode") },
			wantErr: []string{"mode is required once set"},
		},
		{
			name: "mode set on a VolumeSnapshot created without it",
			spec: func() map[string]any {
				spec := captureSpec()
				delete(spec, "mode")
				return spec
			},
			change: func(spec map[string]any) { spec["mode"] = "Capture" },
		},
		{
			name:    "persistentVolumeClaimName",
			spec:    captureSpec,
			change:  func(spec map[string]any) { spec["source"] = map[string]any{"persistentVolumeClaimName": "other"} },
			wantErr: []string{"persistentVolumeClaimName is immutable"},
		},
		{
			name:    "persistentVolumeClaimName removed",
			spec:    captureSpec,
			change:  func(spec map[string]any) { spec["source"] = map[string]any{} },
			wantErr: []string{"persistentVolumeClaimName is required once set"},
		},
		{
			name:    "volumeSnapshotContentName",
			spec:    preProvisionedSpec,
			change:  func(spec map[string]any) { spec["source"] = map[string]any{"volumeSnapshotContentName": "other"} },
			wantErr: []string{"volumeSnapshotContentName is immutable once set"},
		},
		{
			name:    "volumeSnapshotContentName removed",
			spec:    preProvisionedSpec,
			change:  func(spec map[string]any) { spec["source"] = map[string]any{} },
			wantErr: []string{"volumeSnapshotContentName is required once set"},
		},
		{
			name: "volumeSnapshotContentName set on an empty source",
			spec: restoreIntentSpec,
			change: func(spec map[string]any) {
				spec["source"] = map[string]any{"volumeSnapshotContentName": "snapcontent"}
			},
		},
		{
			name: "volumeSnapshotContentName set once from the empty string",
			spec: func() map[string]any {
				spec := restoreIntentSpec()
				spec["source"] = map[string]any{"volumeSnapshotContentName": ""}
				return spec
			},
			change: func(spec map[string]any) {
				spec["source"] = map[string]any{"volumeSnapshotContentName": "snapcontent"}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			vs := volumeSnapshot(ns, objectName("update", tt.name), tt.spec())
			createObject(t, vs)
			spec, _, err := unstructured.NestedMap(vs.Object, "spec")
			if err != nil {
				t.Fatalf("read spec: %v", err)
			}
			tt.change(spec)
			vs.Object["spec"] = spec
			expectAdmission(t, k8sClient.Update(context.Background(), vs), tt.wantErr...)
		})
	}
}
