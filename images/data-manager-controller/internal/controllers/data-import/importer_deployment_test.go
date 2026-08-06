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

package dataimport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

const (
	importerDeploymentTestNamespace  = "ns"
	importerDeploymentTestImportName = "imp-1"
	importerDeploymentControllerNS   = "d8"
)

// importerDeploymentScheme carries every typed API group ensureImporterDeployment/importerPodsGone/
// stopImporter touch: corev1 (ConfigMap, PVC, Pod) and appsv1 (Deployment).
func importerDeploymentScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	return scheme
}

// newImporterDeploymentReconciler builds a DataImportReconciler wired the same way Reconcile wires it
// (r.dataImport + r.names set from the DataImport identity), plus the image ConfigMap MakeServerContainer
// needs, so ensureImporterDeployment/importerPodsGone/stopImporter can be exercised directly without
// driving the whole Reconcile.
func newImporterDeploymentReconciler(t *testing.T, extraObjects []client.Object, interceptorFuncs ...interceptor.Funcs) (*DataImportReconciler, *dev1alpha1.DataImport) {
	t.Helper()

	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: importerDeploymentTestImportName, Namespace: importerDeploymentTestNamespace},
		Spec:       dev1alpha1.DataImportSpec{Mode: dev1alpha1.DataImportModePopulateData, Ttl: "30m"},
	}

	objs := append([]client.Object{exporterImageConfigMap(importerDeploymentControllerNS)}, extraObjects...)
	builder := fake.NewClientBuilder().WithScheme(importerDeploymentScheme(t)).WithObjects(objs...)
	for _, f := range interceptorFuncs {
		builder = builder.WithInterceptorFuncs(f)
	}
	c := builder.Build()

	r := &DataImportReconciler{
		Client:     c,
		Config:     &config.Options{ControllerNamespace: importerDeploymentControllerNS},
		dataImport: di,
		names:      common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name),
	}
	return r, di
}

// scratchPVCFixture builds the internal scratch PVC ensureImporterDeployment mounts, named after the
// DataImport (as ensureScratchPVC would produce it), with the given volume mode.
func scratchPVCFixture(mode corev1.PersistentVolumeMode) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      importerDeploymentTestImportName,
			Namespace: importerDeploymentControllerNS,
			UID:       types.UID("scratch-pvc-uid"),
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeMode: &mode},
	}
}

// TestEnsureImporterDeployment_MountsInternalPVCInControllerNamespace guards the shape of the Deployment
// ensureImporterDeployment produces: it must mount the internal scratch PVC (not any user-namespace PVC),
// live in the controller namespace, run the server in import mode with the PVC's own volume mode, carry
// the pod-template labels the Pod-List/watch contracts (importerPodsGone, cmd/main.go) depend on, and
// stamp the DataImport's own namespace/name onto the storage-manager annotations (server_pod.go's mapping
// contract), not the controller namespace the Deployment itself lives in.
func TestEnsureImporterDeployment_MountsInternalPVCInControllerNamespace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		volumeMode corev1.PersistentVolumeMode
		wantMode   string
	}{
		{name: "success: Filesystem volume mode", volumeMode: corev1.PersistentVolumeFilesystem, wantMode: "--mode=filesystem"},
		{name: "success: Block volume mode", volumeMode: corev1.PersistentVolumeBlock, wantMode: "--mode=block"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pvc := scratchPVCFixture(tt.volumeMode)
			r, di := newImporterDeploymentReconciler(t, nil)

			err := r.ensureImporterDeployment(context.Background(), pvc)
			require.NoError(t, err)

			deploy := &appsv1.Deployment{}
			require.NoError(t, r.Client.Get(context.Background(),
				types.NamespacedName{Namespace: importerDeploymentControllerNS, Name: r.names.DeployName}, deploy))

			podSpec := deploy.Spec.Template.Spec
			require.Len(t, podSpec.Volumes, 1)
			require.NotNil(t, podSpec.Volumes[0].PersistentVolumeClaim)
			assert.Equal(t, pvc.Name, podSpec.Volumes[0].PersistentVolumeClaim.ClaimName)

			require.Len(t, podSpec.Containers, 1)
			assert.Contains(t, podSpec.Containers[0].Args, "--operation=import")
			assert.Contains(t, podSpec.Containers[0].Args, tt.wantMode)

			assert.Equal(t, dev1alpha1.LabelDataImportValue, deploy.Spec.Template.Labels[dev1alpha1.LabelApplicationKey])
			assert.Equal(t, r.names.DeployName, deploy.Spec.Template.Labels[dev1alpha1.LabelStorageManagerDeploymentNameKey])

			// The mapping annotations point back at the DataImport's OWN namespace/name, not the controller
			// namespace the Deployment itself lives in -- this is what server_pod.go relies on.
			assert.Equal(t, di.Namespace, deploy.Spec.Template.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey])
			assert.Equal(t, di.Name, deploy.Spec.Template.Annotations[dev1alpha1.AnnotationStorageManagerNameKey])

			assert.Equal(t, common.ServiceAccountServer, podSpec.ServiceAccountName)
		})
	}
}

// TestEnsureImporterDeployment_IsIdempotent guards the upload-phase requeue loop (Step 5 calls
// ensureImporterDeployment on every reconcile while awaiting bind/upload): a second call against
// unchanged inputs must not issue any further write.
func TestEnsureImporterDeployment_IsIdempotent(t *testing.T) {
	t.Parallel()

	pvc := scratchPVCFixture(corev1.PersistentVolumeFilesystem)
	var creates, updates int
	r, _ := newImporterDeploymentReconciler(t, nil, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				creates++
			}
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				updates++
			}
			return cl.Update(ctx, obj, opts...)
		},
	})

	require.NoError(t, r.ensureImporterDeployment(context.Background(), pvc))
	require.NoError(t, r.ensureImporterDeployment(context.Background(), pvc))

	assert.Equal(t, 1, creates, "the Deployment must be created exactly once")
	assert.Equal(t, 0, updates, "an unchanged Deployment must not be re-written on the second call")
}

// TestEnsureImporterDeployment_ErrorOnNilVolumeMode guards the documented nil-guard: a scratch PVC
// without spec.volumeMode set must surface a plain error, not dereference a nil pointer.
func TestEnsureImporterDeployment_ErrorOnNilVolumeMode(t *testing.T) {
	t.Parallel()

	t.Run("error: scratch PVC has no volume mode", func(t *testing.T) {
		t.Parallel()

		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: importerDeploymentTestImportName, Namespace: importerDeploymentControllerNS},
		}
		r, _ := newImporterDeploymentReconciler(t, nil)

		err := r.ensureImporterDeployment(context.Background(), pvc)
		require.Error(t, err)
	})
}

// importerPodFixture builds a Pod carrying the importer's watch/list labels, so tests can place it in
// (or out of) the set importerPodsGone looks for.
func importerPodFixture(name, namespace, deployNameLabel string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				dev1alpha1.LabelApplicationKey:                  dev1alpha1.LabelDataImportValue,
				dev1alpha1.LabelStorageManagerDeploymentNameKey: deployNameLabel,
			},
		},
	}
}

// TestImporterPodsGone covers the pod-presence classification importerPodsGone/stopImporter rely on as
// the "volume was unmounted" signal.
func TestImporterPodsGone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		buildPod func(deployName string) *corev1.Pod
		wantGone bool
	}{
		{
			name:     "success: no pods at all reports gone",
			buildPod: nil,
			wantGone: true,
		},
		{
			name: "success: a matching-label pod in the controller namespace reports not gone",
			buildPod: func(deployName string) *corev1.Pod {
				return importerPodFixture("importer-pod", importerDeploymentControllerNS, deployName)
			},
			wantGone: false,
		},
		{
			name: "success: right app label but a different deployment-name label is not confused with this import",
			buildPod: func(_ string) *corev1.Pod {
				return importerPodFixture("other-import-pod", importerDeploymentControllerNS, "deploy-for-some-other-import")
			},
			wantGone: true,
		},
		{
			name: "success: matching labels in a different namespace do not count",
			buildPod: func(deployName string) *corev1.Pod {
				return importerPodFixture("cross-namespace-pod", "other-namespace", deployName)
			},
			wantGone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, _ := newImporterDeploymentReconciler(t, nil)

			var extra []client.Object
			if tt.buildPod != nil {
				extra = append(extra, tt.buildPod(r.names.DeployName))
			}
			for _, obj := range extra {
				require.NoError(t, r.Client.Create(context.Background(), obj))
			}

			gone, err := r.importerPodsGone(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.wantGone, gone)
		})
	}
}

// TestStopImporter covers the delete-and-confirm contract: stopImporter must delete the Deployment and
// only report stopped once no importer pod remains either.
func TestStopImporter(t *testing.T) {
	t.Parallel()

	t.Run("success: Deployment and pod present -> not stopped, Deployment deleted", func(t *testing.T) {
		t.Parallel()

		pvc := scratchPVCFixture(corev1.PersistentVolumeFilesystem)
		r, _ := newImporterDeploymentReconciler(t, nil)
		require.NoError(t, r.ensureImporterDeployment(context.Background(), pvc))

		pod := importerPodFixture("importer-pod", importerDeploymentControllerNS, r.names.DeployName)
		require.NoError(t, r.Client.Create(context.Background(), pod))

		stopped, err := r.stopImporter(context.Background())
		require.NoError(t, err)
		assert.False(t, stopped)

		deploy := &appsv1.Deployment{}
		getErr := r.Client.Get(context.Background(),
			types.NamespacedName{Namespace: importerDeploymentControllerNS, Name: r.names.DeployName}, deploy)
		require.Error(t, getErr, "the Deployment must be deleted even though the pod is still around")

		t.Run("success: pod gone on second call -> stopped", func(t *testing.T) {
			require.NoError(t, r.Client.Delete(context.Background(), pod))

			stopped, err := r.stopImporter(context.Background())
			require.NoError(t, err)
			assert.True(t, stopped)
		})
	})

	t.Run("success: Deployment absent from the start -> stopped, no error", func(t *testing.T) {
		t.Parallel()

		r, _ := newImporterDeploymentReconciler(t, nil)

		stopped, err := r.stopImporter(context.Background())
		require.NoError(t, err)
		assert.True(t, stopped)
	})
}
