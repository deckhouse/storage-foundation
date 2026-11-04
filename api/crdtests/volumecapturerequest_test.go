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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

func snapshotCapture(namespace, name string) *v1alpha1.VolumeCaptureRequest {
	return &v1alpha1.VolumeCaptureRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.VolumeCaptureRequestSpec{
			Mode: v1alpha1.VolumeCaptureModeSnapshot,
			Target: &v1alpha1.VolumeCaptureTarget{
				UID:        "pvc-uid",
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       "data",
			},
		},
	}
}

// Server-side immutability of the spec is not enforced yet (see the doc
// comments of VolumeCaptureTarget), so there are no update cases.
func TestVolumeCaptureRequestCreate(t *testing.T) {
	ns := createNamespace(t, "volumecapturerequest-create")

	for _, tt := range []struct {
		name    string
		vcr     func(namespace, name string) client.Object
		wantErr []string
	}{
		{name: "Snapshot", vcr: func(ns, n string) client.Object { return snapshotCapture(ns, n) }},
		{
			name: "Detach without target",
			vcr: func(ns, n string) client.Object {
				vcr := snapshotCapture(ns, n)
				vcr.Spec.Mode = v1alpha1.VolumeCaptureModeDetach
				vcr.Spec.Target = nil
				return vcr
			},
		},
		{
			name: "Snapshot without target",
			vcr: func(ns, n string) client.Object {
				vcr := snapshotCapture(ns, n)
				vcr.Spec.Target = nil
				return vcr
			},
			wantErr: []string{"spec.target is required when mode is Snapshot"},
		},
		{
			name: "unknown mode",
			vcr: func(ns, n string) client.Object {
				vcr := snapshotCapture(ns, n)
				vcr.Spec.Mode = "Clone"
				return vcr
			},
			wantErr: []string{"spec.mode", "Unsupported value"},
		},
		{
			name: "without mode",
			vcr: func(ns, n string) client.Object {
				vcr := toUnstructured(snapshotCapture(ns, n))
				unstructured.RemoveNestedField(vcr.Object, "spec", "mode")
				return vcr
			},
			wantErr: []string{"spec.mode", "Required value"},
		},
		{
			name: "empty target.uid",
			vcr: func(ns, n string) client.Object {
				vcr := snapshotCapture(ns, n)
				vcr.Spec.Target.UID = ""
				return vcr
			},
			wantErr: []string{"spec.target.uid", "should be at least 1 chars long"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			expectCreate(t, tt.vcr(ns, objectName("create", tt.name)), tt.wantErr...)
		})
	}
}
