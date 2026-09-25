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

package common

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common/goldentest"
)

// Deterministic inputs so the rendered container -- and therefore the golden file -- is stable across runs.
const (
	serverGoldenControllerNamespace = "d8-storage-foundation"
	serverGoldenResourceNamespace   = "test-ns"
	serverGoldenResourceName        = "test-di"
	serverGoldenTargetPVCName       = "test-pvc"
	serverGoldenImage               = "registry.example.com/storage/data-exporter:golden"
	serverGoldenTTL                 = "10m"
)

func buildServerContainer(t *testing.T, mode corev1.PersistentVolumeMode) *corev1.Container {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: serverGoldenControllerNamespace,
			Name:      CongigMapName,
		},
		Data: map[string]string{"image": serverGoldenImage},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	names := NewNames(dev1alpha1.KindPVC, serverGoldenTargetPVCName, serverGoldenResourceNamespace, serverGoldenResourceName)

	container, err := MakeServerContainer(context.Background(), cl, ServerContainerCfg{
		ConfigMapName: types.NamespacedName{
			Namespace: serverGoldenControllerNamespace,
			Name:      CongigMapName,
		},
		ResourceName: types.NamespacedName{
			Namespace: serverGoldenResourceNamespace,
			Name:      serverGoldenResourceName,
		},
		VolumeName:          "export-pvc",
		VolumeMode:          mode,
		Ttl:                 serverGoldenTTL,
		ServerMode:          ServerModeImport,
		ControllerNamespace: serverGoldenControllerNamespace,
		Names:               names,
	})
	require.NoError(t, err)

	return container
}

// TestGolden_ServerContainer snapshots the generated server container for both PV modes. Regenerate the
// fixtures after an intentional change with:
//
//	go test . -run TestGolden_ServerContainer -golden
//
// then review the diff in the commit/MR.
func TestGolden_ServerContainer(t *testing.T) {
	cases := []struct {
		name string
		mode corev1.PersistentVolumeMode
	}{
		{name: "filesystem", mode: corev1.PersistentVolumeFilesystem},
		{name: "block", mode: corev1.PersistentVolumeBlock},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			container := buildServerContainer(t, tc.mode)
			got := goldentest.MarshalYAML(t, container)

			goldenPath := filepath.Join("testdata", "golden", "server-container-"+tc.name+".yaml")
			goldentest.CompareOrUpdate(t, goldenPath, got, goldentest.WholeDocument)
		})
	}
}
