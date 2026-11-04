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

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

const (
	populateDataRequires = "mode PopulateData requires snapshotRef and storageParams."
	populateDataForbids  = "mode PopulateData forbids pvcTemplate."
	createPVCRequires    = "mode CreatePVC requires pvcTemplate."
	createPVCForbids     = "mode CreatePVC forbids snapshotRef and storageParams."
	pvcTemplateNameless  = "pvcTemplate requires metadata.name."
)

func pvcTemplate() *v1alpha1.PersistentVolumeClaimTemplateSpec {
	storageClass := "local"
	return &v1alpha1.PersistentVolumeClaimTemplateSpec{
		PersistentVolumeClaimTemplateMetadata: v1alpha1.PersistentVolumeClaimTemplateMetadata{Name: "data"},
		PersistentVolumeClaimSpec: v1alpha1.PersistentVolumeClaimSpec{
			AccessModes: []v1alpha1.PersistentVolumeAccessMode{v1alpha1.ReadWriteOnce},
			Resources: v1alpha1.VolumeResourceRequirements{
				Requests: v1alpha1.ResourceList{"storage": resource.MustParse("1Gi")},
			},
			StorageClassName: &storageClass,
		},
	}
}

func createPVCImport(namespace, name string) *v1alpha1.DataImport {
	return &v1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.DataImportSpec{
			TTL:         "1h",
			Mode:        v1alpha1.DataImportModeCreatePVC,
			PvcTemplate: pvcTemplate(),
		},
	}
}

func populateDataImport(namespace, name string) *v1alpha1.DataImport {
	return &v1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.DataImportSpec{
			TTL:  "1h",
			Mode: v1alpha1.DataImportModePopulateData,
			SnapshotRef: &v1alpha1.ObjectReference{
				APIVersion: "snapshot.storage.k8s.io/v1",
				Kind:       "VolumeSnapshot",
				Name:       "snap",
			},
			StorageParams: &v1alpha1.StorageParamsSpec{StorageClassName: "local", Size: "1Gi"},
		},
	}
}

// createPVCImportWith returns a CreatePVC import as a map with change applied,
// for the fields the typed client cannot leave out or send empty.
func createPVCImportWith(change func(*unstructured.Unstructured)) func(namespace, name string) client.Object {
	return func(namespace, name string) client.Object {
		di := toUnstructured(createPVCImport(namespace, name))
		change(di)
		return di
	}
}

func TestDataImportCreate(t *testing.T) {
	ns := createNamespace(t, "dataimport-create")

	for _, tt := range []struct {
		name    string
		di      func(namespace, name string) client.Object
		wantErr []string
	}{
		{name: "CreatePVC", di: func(ns, n string) client.Object { return createPVCImport(ns, n) }},
		{name: "PopulateData", di: func(ns, n string) client.Object { return populateDataImport(ns, n) }},
		{
			name: "PopulateData without snapshotRef",
			di: func(ns, n string) client.Object {
				di := populateDataImport(ns, n)
				di.Spec.SnapshotRef = nil
				return di
			},
			wantErr: []string{populateDataRequires},
		},
		{
			name: "PopulateData without storageParams",
			di: func(ns, n string) client.Object {
				di := populateDataImport(ns, n)
				di.Spec.StorageParams = nil
				return di
			},
			wantErr: []string{populateDataRequires},
		},
		{
			name: "PopulateData with pvcTemplate",
			di: func(ns, n string) client.Object {
				di := populateDataImport(ns, n)
				di.Spec.PvcTemplate = pvcTemplate()
				return di
			},
			wantErr: []string{populateDataForbids},
		},
		{
			name: "CreatePVC without pvcTemplate",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				di.Spec.PvcTemplate = nil
				return di
			},
			wantErr: []string{createPVCRequires},
		},
		{
			name: "mode defaults to CreatePVC",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				di.Spec.Mode = ""
				di.Spec.PvcTemplate = nil
				return di
			},
			wantErr: []string{createPVCRequires},
		},
		{
			name: "CreatePVC with snapshotRef",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				di.Spec.SnapshotRef = populateDataImport(ns, n).Spec.SnapshotRef
				return di
			},
			wantErr: []string{createPVCForbids},
		},
		{
			name: "CreatePVC with storageParams",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				di.Spec.StorageParams = populateDataImport(ns, n).Spec.StorageParams
				return di
			},
			wantErr: []string{createPVCForbids},
		},
		{
			name: "pvcTemplate without metadata",
			di: createPVCImportWith(func(di *unstructured.Unstructured) {
				unstructured.RemoveNestedField(di.Object, "spec", "pvcTemplate", "metadata")
			}),
			wantErr: []string{pvcTemplateNameless},
		},
		{
			name: "pvcTemplate without metadata.name",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				di.Spec.PvcTemplate.Name = ""
				return di
			},
			wantErr: []string{pvcTemplateNameless},
		},
		{
			name: "pvcTemplate with an empty metadata.name",
			di: createPVCImportWith(func(di *unstructured.Unstructured) {
				setField(di, "", "spec", "pvcTemplate", "metadata", "name")
			}),
			wantErr: []string{pvcTemplateNameless},
		},
		{
			name: "unknown mode",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				di.Spec.Mode = "Copy"
				return di
			},
			wantErr: []string{"spec.mode", "Unsupported value"},
		},
		{
			name: "ttl in days",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				di.Spec.TTL = "1d"
				return di
			},
			wantErr: []string{"spec.ttl", "should match"},
		},
		{
			name: "unknown pvcTemplate access mode",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				di.Spec.PvcTemplate.AccessModes = []v1alpha1.PersistentVolumeAccessMode{"ReadWriteSometimes"}
				return di
			},
			wantErr: []string{"spec.pvcTemplate.spec.accessModes[0]", "Unsupported value"},
		},
		{
			name: "unknown pvcTemplate volumeMode",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				mode := v1alpha1.PersistentVolumeMode("Raw")
				di.Spec.PvcTemplate.VolumeMode = &mode
				return di
			},
			wantErr: []string{"spec.pvcTemplate.spec.volumeMode", "Unsupported value"},
		},
		{
			name: "empty pvcTemplate storageClassName",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				empty := ""
				di.Spec.PvcTemplate.StorageClassName = &empty
				return di
			},
			wantErr: []string{"spec.pvcTemplate.spec.storageClassName", "should be at least 1 chars long"},
		},
		{
			name: "pvcTemplate without storageClassName",
			di: func(ns, n string) client.Object {
				di := createPVCImport(ns, n)
				di.Spec.PvcTemplate.StorageClassName = nil
				return di
			},
		},
		{
			name: "snapshotRef without kind",
			di: func(ns, n string) client.Object {
				di := populateDataImport(ns, n)
				di.Spec.SnapshotRef.Kind = ""
				return di
			},
			wantErr: []string{"spec.snapshotRef.kind", "Required value"},
		},
		{
			name: "empty snapshotRef.name",
			di: func(ns, n string) client.Object {
				di := populateDataImport(ns, n)
				di.Spec.SnapshotRef.Name = ""
				return di
			},
			wantErr: []string{"spec.snapshotRef.name", "should be at least 1 chars long"},
		},
		{
			name: "empty storageParams.size",
			di: func(ns, n string) client.Object {
				di := populateDataImport(ns, n)
				di.Spec.StorageParams.Size = ""
				return di
			},
			wantErr: []string{"spec.storageParams.size", "should be at least 1 chars long"},
		},
		{
			name: "unknown storageParams.volumeMode",
			di: func(ns, n string) client.Object {
				di := populateDataImport(ns, n)
				di.Spec.StorageParams.VolumeMode = "Raw"
				return di
			},
			wantErr: []string{"spec.storageParams.volumeMode", "Unsupported value"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			expectCreate(t, tt.di(ns, objectName("create", tt.name)), tt.wantErr...)
		})
	}
}

func TestDataImportUpdate(t *testing.T) {
	ns := createNamespace(t, "dataimport-update")

	for _, tt := range []struct {
		name    string
		di      func(namespace, name string) *v1alpha1.DataImport
		change  func(*v1alpha1.DataImport)
		wantErr []string
	}{
		{
			name: "mode",
			di:   createPVCImport,
			change: func(di *v1alpha1.DataImport) {
				populate := populateDataImport(di.Namespace, di.Name)
				di.Spec.Mode = populate.Spec.Mode
				di.Spec.PvcTemplate = nil
				di.Spec.SnapshotRef = populate.Spec.SnapshotRef
				di.Spec.StorageParams = populate.Spec.StorageParams
			},
			wantErr: []string{"mode is immutable"},
		},
		{
			name:    "snapshotRef.name",
			di:      populateDataImport,
			change:  func(di *v1alpha1.DataImport) { di.Spec.SnapshotRef.Name = "other" },
			wantErr: []string{"spec.snapshotRef", "snapshotRef is immutable"},
		},
		{
			name: "snapshotRef.kind",
			di:   populateDataImport,
			change: func(di *v1alpha1.DataImport) {
				di.Spec.SnapshotRef.APIVersion = "virtualization.deckhouse.io/v1alpha2"
				di.Spec.SnapshotRef.Kind = "VirtualDiskSnapshot"
			},
			wantErr: []string{"spec.snapshotRef", "snapshotRef is immutable"},
		},
		{
			name: "ttl, publish, waitForFirstConsumer and pvcTemplate are mutable",
			di:   createPVCImport,
			change: func(di *v1alpha1.DataImport) {
				di.Spec.TTL = "30m"
				di.Spec.Publish = true
				di.Spec.WaitForFirstConsumer = false
				di.Spec.PvcTemplate.Name = "other"
				di.Spec.PvcTemplate.Labels = map[string]string{"app": "db"}
			},
		},
		{
			name: "storageParams is mutable",
			di:   populateDataImport,
			change: func(di *v1alpha1.DataImport) {
				di.Spec.StorageParams = &v1alpha1.StorageParamsSpec{StorageClassName: "other", Size: "2Gi", VolumeMode: "Block"}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			di := tt.di(ns, objectName("update", tt.name))
			createObject(t, di)
			tt.change(di)
			expectAdmission(t, k8sClient.Update(context.Background(), di), tt.wantErr...)
		})
	}
}
