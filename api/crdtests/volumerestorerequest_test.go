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

func restoreRequest(namespace, name string) *v1alpha1.VolumeRestoreRequest {
	return &v1alpha1.VolumeRestoreRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.VolumeRestoreRequestSpec{
			SourceRef: v1alpha1.ObjectReference{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshotContent",
				Name:       "snapcontent",
			},
			PvcTemplate: *pvcTemplate(),
		},
	}
}

// restoreRequestWith returns a VolumeRestoreRequest as a map with change
// applied, for the fields the typed client cannot leave out or send empty.
func restoreRequestWith(change func(*unstructured.Unstructured)) func(namespace, name string) client.Object {
	return func(namespace, name string) client.Object {
		vrr := toUnstructured(restoreRequest(namespace, name))
		change(vrr)
		return vrr
	}
}

func TestVolumeRestoreRequestCreate(t *testing.T) {
	ns := createNamespace(t, "volumerestorerequest-create")

	for _, tt := range []struct {
		name    string
		vrr     func(namespace, name string) client.Object
		wantErr []string
	}{
		{name: "VolumeSnapshotContent", vrr: func(ns, n string) client.Object { return restoreRequest(ns, n) }},
		{
			name: "pvcTemplate without metadata.name",
			vrr: func(ns, n string) client.Object {
				vrr := restoreRequest(ns, n)
				vrr.Spec.PvcTemplate.Name = ""
				return vrr
			},
			wantErr: []string{"pvcTemplate.metadata.name is required"},
		},
		{
			name: "pvcTemplate without metadata",
			vrr: restoreRequestWith(func(vrr *unstructured.Unstructured) {
				unstructured.RemoveNestedField(vrr.Object, "spec", "pvcTemplate", "metadata")
			}),
			wantErr: []string{"pvcTemplate.metadata.name is required"},
		},
		{
			name: "empty pvcTemplate metadata.name",
			vrr: restoreRequestWith(func(vrr *unstructured.Unstructured) {
				setField(vrr, "", "spec", "pvcTemplate", "metadata", "name")
			}),
			wantErr: []string{"pvcTemplate.metadata.name is required"},
		},
		{
			name: "without sourceRef",
			vrr: restoreRequestWith(func(vrr *unstructured.Unstructured) {
				unstructured.RemoveNestedField(vrr.Object, "spec", "sourceRef")
			}),
			wantErr: []string{"spec.sourceRef", "Required value"},
		},
		{
			name: "without sourceRef.name",
			vrr: restoreRequestWith(func(vrr *unstructured.Unstructured) {
				unstructured.RemoveNestedField(vrr.Object, "spec", "sourceRef", "name")
			}),
			wantErr: []string{"spec.sourceRef.name", "Required value"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			expectCreate(t, tt.vrr(ns, objectName("create", tt.name)), tt.wantErr...)
		})
	}
}
