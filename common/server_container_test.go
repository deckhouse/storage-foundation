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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

func imageConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "images", Namespace: "d8-ns"},
		Data:       map[string]string{"image": "registry.example/data-exporter:v1"},
	}
}

// TestMakeServerContainer_TTLPlumbed is the direct guard for the plan's finding that spec.ttl must reach
// the server pod: the exporter/importer's idle-TTL is enforced by --ttl, and cfg.Ttl (populated from
// DataImport/DataExport spec.ttl) must appear verbatim in the container args.
func TestMakeServerContainer_TTLPlumbed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(imageConfigMap()).Build()

	cfg := ServerContainerCfg{
		ConfigMapName: types.NamespacedName{Name: "images", Namespace: "d8-ns"},
		ResourceName:  types.NamespacedName{Name: "imp", Namespace: "app"},
		VolumeName:    "vol",
		VolumeMode:    corev1.PersistentVolumeFilesystem,
		Ttl:           "30m",
		ServerMode:    ServerModeImport,
		Names:         NewNames(dev1alpha1.KindPVC, "data", "app", "imp"),
	}

	container, err := MakeServerContainer(context.Background(), c, cfg)
	require.NoError(t, err)
	require.NotNil(t, container)

	assert.Contains(t, container.Args, "--ttl=30m", "spec.ttl must be plumbed to the server as --ttl")
	assert.Contains(t, container.Args, "--operation=import")
	assert.Contains(t, container.Args, "--mode=filesystem")
	assert.Equal(t, "registry.example/data-exporter:v1", container.Image)
	// Filesystem mode mounts a volume (not a raw device).
	assert.Len(t, container.VolumeMounts, 1)
	assert.Empty(t, container.VolumeDevices)
}

func TestMakeServerContainer_BlockMode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(imageConfigMap()).Build()

	cfg := ServerContainerCfg{
		ConfigMapName: types.NamespacedName{Name: "images", Namespace: "d8-ns"},
		ResourceName:  types.NamespacedName{Name: "exp", Namespace: "app"},
		VolumeName:    "vol",
		VolumeMode:    corev1.PersistentVolumeBlock,
		Ttl:           "1h",
		ServerMode:    ServerModeExport,
		Names:         NewNames(dev1alpha1.KindPVC, "data", "app", "exp"),
	}

	container, err := MakeServerContainer(context.Background(), c, cfg)
	require.NoError(t, err)

	assert.Contains(t, container.Args, "--ttl=1h")
	assert.Contains(t, container.Args, "--operation=export")
	assert.Contains(t, container.Args, "--mode=block")
	// Block mode attaches a raw device (not a filesystem mount).
	assert.Len(t, container.VolumeDevices, 1)
	assert.Empty(t, container.VolumeMounts)
}

// TestMakeServerContainer_MissingImage surfaces a misconfigured images ConfigMap as an error rather than
// silently building a container with an empty image.
func TestMakeServerContainer_MissingImage(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build() // no ConfigMap

	_, err := MakeServerContainer(context.Background(), c, ServerContainerCfg{
		ConfigMapName: types.NamespacedName{Name: "images", Namespace: "d8-ns"},
		VolumeMode:    corev1.PersistentVolumeFilesystem,
		Ttl:           "30m",
		ServerMode:    ServerModeImport,
	})
	require.Error(t, err)
}
