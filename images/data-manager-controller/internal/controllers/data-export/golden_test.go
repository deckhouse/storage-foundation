/*
Copyright 2025 Flant JSC

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

package dataexport

import (
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/goldentest"
)

// Deterministic inputs so the rendered manifest — and therefore the golden
// file — is stable across runs.
const (
	goldenControllerNamespace = "d8-storage-foundation"
	goldenImage               = "registry.example.com/storage/data-exporter:golden"
	goldenTTL                 = "10m"
)

// buildExportDeployment renders the DataExport server Deployment exactly the way
// createDeployment does, so the golden snapshot captures the full object the
// controller would submit to the API server (including the pod SecurityContext).
func buildExportDeployment(mode corev1.PersistentVolumeMode) appsv1.Deployment {
	dataExport := createDataExport(dev1alpha1.DataExportSpec{TTL: goldenTTL})
	names := common.NewNames(dev1alpha1.KindPVC, testUserPVCName, dataExportNamespace, dataExportName)

	return appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.DeployName,
			Namespace: goldenControllerNamespace,
			Labels: map[string]string{
				dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue,
			},
		},
		Spec: makeExportDeploySpec(&mode, dataExport, goldenImage, dataExport.Spec.TTL, goldenControllerNamespace, names),
	}
}

// TestGolden_ExportDeployment snapshots the generated server Deployment for both
// PV modes. Regenerate the fixtures after an intentional change with:
//
//	go test ./internal/controllers/data-export/ -run TestGolden -golden
//
// then review the diff in the commit/MR.
func TestGolden_ExportDeployment(t *testing.T) {
	cases := []struct {
		name string
		mode corev1.PersistentVolumeMode
	}{
		{name: "filesystem", mode: corev1.PersistentVolumeFilesystem},
		{name: "block", mode: corev1.PersistentVolumeBlock},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deploy := buildExportDeployment(tc.mode)
			got := goldentest.MarshalYAML(t, deploy)

			goldenPath := filepath.Join("testdata", "golden", "export-deployment-"+tc.name+".yaml")
			goldentest.CompareOrUpdate(t, goldenPath, got, goldentest.WholeDocument)
		})
	}
}
