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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

const targetRefImmutable = "spec.targetRef is immutable"

func pvcExport(namespace, name string) *v1alpha1.DataExport {
	return &v1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.DataExportSpec{
			TTL:       "1h",
			TargetRef: v1alpha1.DataExportTargetRefSpec{Kind: "PersistentVolumeClaim", Name: "data"},
		},
	}
}

func snapshotExport(namespace, name string) *v1alpha1.DataExport {
	de := pvcExport(namespace, name)
	de.Spec.TargetRef = v1alpha1.DataExportTargetRefSpec{
		Group: "snapshot.storage.k8s.io",
		Kind:  "VolumeSnapshot",
		Name:  "snap",
	}
	return de
}

func TestDataExportCreate(t *testing.T) {
	ns := createNamespace(t, "dataexport-create")

	for _, tt := range []struct {
		name    string
		de      func(namespace, name string) *v1alpha1.DataExport
		wantErr []string
	}{
		{name: "PersistentVolumeClaim", de: pvcExport},
		{name: "VolumeSnapshot", de: snapshotExport},
		{
			name: "published through the console",
			de: func(namespace, name string) *v1alpha1.DataExport {
				de := pvcExport(namespace, name)
				de.Spec.Publish = true
				de.Spec.PublicIngress = "ConsoleFrontend"
				return de
			},
		},
		{
			name: "compound ttl",
			de: func(namespace, name string) *v1alpha1.DataExport {
				de := pvcExport(namespace, name)
				de.Spec.TTL = "2h45m30s"
				return de
			},
		},
		{
			name: "ttl in days",
			de: func(namespace, name string) *v1alpha1.DataExport {
				de := pvcExport(namespace, name)
				de.Spec.TTL = "1d"
				return de
			},
			wantErr: []string{"spec.ttl", "should match"},
		},
		{
			name: "unknown publicIngress",
			de: func(namespace, name string) *v1alpha1.DataExport {
				de := pvcExport(namespace, name)
				de.Spec.PublicIngress = "Ingress"
				return de
			},
			wantErr: []string{"spec.publicIngress", "Unsupported value"},
		},
		{
			name: "empty targetRef.kind",
			de: func(namespace, name string) *v1alpha1.DataExport {
				de := pvcExport(namespace, name)
				de.Spec.TargetRef.Kind = ""
				return de
			},
			wantErr: []string{"spec.targetRef.kind", "should be at least 1 chars long"},
		},
		{
			name: "empty targetRef.name",
			de: func(namespace, name string) *v1alpha1.DataExport {
				de := pvcExport(namespace, name)
				de.Spec.TargetRef.Name = ""
				return de
			},
			wantErr: []string{"spec.targetRef.name", "should be at least 1 chars long"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			expectCreate(t, tt.de(ns, objectName("create", tt.name)), tt.wantErr...)
		})
	}
}

func TestDataExportUpdate(t *testing.T) {
	ns := createNamespace(t, "dataexport-update")

	for _, tt := range []struct {
		name    string
		de      func(namespace, name string) *v1alpha1.DataExport
		change  func(*v1alpha1.DataExport)
		wantErr []string
	}{
		{
			name:    "targetRef.name",
			de:      pvcExport,
			change:  func(de *v1alpha1.DataExport) { de.Spec.TargetRef.Name = "other" },
			wantErr: []string{targetRefImmutable},
		},
		{
			name:    "targetRef.kind",
			de:      snapshotExport,
			change:  func(de *v1alpha1.DataExport) { de.Spec.TargetRef.Kind = "VirtualDiskSnapshot" },
			wantErr: []string{targetRefImmutable},
		},
		{
			name:    "targetRef.group added",
			de:      pvcExport,
			change:  func(de *v1alpha1.DataExport) { de.Spec.TargetRef.Group = "virtualization.deckhouse.io" },
			wantErr: []string{targetRefImmutable},
		},
		{
			name:    "targetRef.group removed",
			de:      snapshotExport,
			change:  func(de *v1alpha1.DataExport) { de.Spec.TargetRef.Group = "" },
			wantErr: []string{targetRefImmutable},
		},
		{
			name: "ttl, publish and publicIngress are mutable",
			de:   pvcExport,
			change: func(de *v1alpha1.DataExport) {
				de.Spec.TTL = "30m"
				de.Spec.Publish = true
				de.Spec.PublicIngress = "KubernetesAPI"
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			de := tt.de(ns, objectName("update", tt.name))
			createObject(t, de)
			tt.change(de)
			expectAdmission(t, k8sClient.Update(context.Background(), de), tt.wantErr...)
		})
	}
}

// TestDataExportUpdateEmptyGroup covers the normalisation the targetRef rule
// exists for: a create that sent group "" comes back from the controller's Go
// struct with the group omitted, and that write must not count as a change of
// the export target.
func TestDataExportUpdateEmptyGroup(t *testing.T) {
	ns := createNamespace(t, "dataexport-empty-group")

	de := toUnstructured(pvcExport(ns, "empty-group"))
	setField(de, "", "spec", "targetRef", "group")
	createObject(t, de)

	unstructured.RemoveNestedField(de.Object, "spec", "targetRef", "group")
	expectAdmission(t, k8sClient.Update(context.Background(), de))
}
