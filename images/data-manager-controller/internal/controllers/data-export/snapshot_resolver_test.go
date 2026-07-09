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

package dataexport

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

const testControllerNamespace = "controller-ns"

var volumeSnapshotLeafGVR = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}

func TestClassifyTargetRef(t *testing.T) {
	tests := []struct {
		name      string
		group     string
		kind      string
		wantCat   targetCategory
		wantShort string
		wantErr   bool
	}{
		{name: "empty kind is rejected", group: "", kind: "", wantErr: true},
		{name: "core PVC -> live PVC", group: "", kind: "PersistentVolumeClaim", wantCat: categoryLivePVC, wantShort: dev1alpha1.KindPVCShort},
		{name: "VirtualDisk -> live VirtualDisk", group: "virtualization.deckhouse.io", kind: "VirtualDisk", wantCat: categoryLiveVirtualDisk, wantShort: dev1alpha1.KindVirtualDiskShort},
		{name: "bare VolumeSnapshotContent is rejected", group: "snapshot.storage.k8s.io", kind: "VolumeSnapshotContent", wantErr: true},
		{name: "VolumeSnapshot leaf -> snapshot", group: "snapshot.storage.k8s.io", kind: "VolumeSnapshot", wantCat: categorySnapshot, wantShort: dev1alpha1.KindSnapshotShort},
		{name: "VirtualDiskSnapshot leaf -> snapshot", group: "virtualization.deckhouse.io", kind: "VirtualDiskSnapshot", wantCat: categorySnapshot, wantShort: dev1alpha1.KindSnapshotShort},
		{name: "arbitrary domain snapshot -> snapshot", group: "example.deckhouse.io", kind: "FancySnapshot", wantCat: categorySnapshot, wantShort: dev1alpha1.KindSnapshotShort},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, short, err := classifyTargetRef(tt.group, tt.kind)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCat, cat)
			assert.Equal(t, tt.wantShort, short)
		})
	}
}

func TestVolumeRestoreRequestName(t *testing.T) {
	names := common.NewNamesFromShort(dev1alpha1.KindSnapshotShort, "leaf1", "test-ns", "de1")
	got := volumeRestoreRequestName(names)

	assert.Equal(t, got, volumeRestoreRequestName(names), "name must be deterministic")
	assert.Contains(t, got, names.TargetKindShort)
	assert.Contains(t, got, names.HashSuffix)
}

func TestVerifySnapshotContentNamespace(t *testing.T) {
	makeContent := func(snapshotRefNS, targetNS string) *unstructured.Unstructured {
		c := &unstructured.Unstructured{Object: map[string]interface{}{}}
		if snapshotRefNS != "" {
			_ = unstructured.SetNestedField(c.Object, snapshotRefNS, "spec", "snapshotRef", "namespace")
		}
		if targetNS != "" {
			_ = unstructured.SetNestedField(c.Object, targetNS, "status", "dataRef", "target", "namespace")
		}
		return c
	}

	tests := []struct {
		name          string
		snapshotRefNS string
		targetNS      string
		wantNamespace string
		wantErr       bool
	}{
		{name: "snapshotRef namespace matches", snapshotRefNS: "test-ns", wantNamespace: "test-ns"},
		{name: "snapshotRef namespace mismatches -> rejected", snapshotRefNS: "victim-ns", wantNamespace: "test-ns", wantErr: true},
		{name: "fallback to target namespace matches", targetNS: "test-ns", wantNamespace: "test-ns"},
		{name: "fallback to target namespace mismatches -> rejected", targetNS: "victim-ns", wantNamespace: "test-ns", wantErr: true},
		{name: "snapshotRef takes precedence over target", snapshotRefNS: "test-ns", targetNS: "victim-ns", wantNamespace: "test-ns"},
		{name: "no anchor recorded -> accepted", wantNamespace: "test-ns"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifySnapshotContentNamespace(makeContent(tt.snapshotRefNS, tt.targetNS), "content1", tt.wantNamespace)
			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrTargetNotFound))
				return
			}
			require.NoError(t, err)
		})
	}
}

// testRESTMapper registers the snapshot leaf (namespaced) and a cluster-scoped resource so the resolver's
// scope guard can be exercised both ways. The default group versions are seeded so the resolver's
// version-less RESTMapping(GroupKind) lookups resolve, mirroring the discovery-backed mapper in prod.
func testRESTMapper() meta.RESTMapper {
	rm := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "snapshot.storage.k8s.io", Version: "v1"},
		{Group: "example.io", Version: "v1"},
	})
	rm.Add(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}, meta.RESTScopeNamespace)
	rm.Add(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "ClusterThing"}, meta.RESTScopeRoot)
	return rm
}

func newSnapshotLeaf(boundContentName string) *unstructured.Unstructured {
	leaf := &unstructured.Unstructured{}
	leaf.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"})
	leaf.SetNamespace("test-ns")
	leaf.SetName("leaf1")
	if boundContentName != "" {
		_ = unstructured.SetNestedField(leaf.Object, boundContentName, "status", "boundSnapshotContentName")
	}
	return leaf
}

func newSnapshotContent(name, snapshotRefNS, artifactKind, artifactName, volumeMode string) *unstructured.Unstructured {
	content := &unstructured.Unstructured{}
	content.SetGroupVersionKind(schema.GroupVersionKind{Group: "state-snapshotter.deckhouse.io", Version: "v1alpha1", Kind: "SnapshotContent"})
	content.SetName(name)
	if snapshotRefNS != "" {
		_ = unstructured.SetNestedField(content.Object, snapshotRefNS, "spec", "snapshotRef", "namespace")
	}
	if artifactKind != "" {
		_ = unstructured.SetNestedField(content.Object, artifactKind, "status", "dataRef", "artifact", "kind")
	}
	if artifactName != "" {
		_ = unstructured.SetNestedField(content.Object, artifactName, "status", "dataRef", "artifact", "name")
	}
	if volumeMode != "" {
		_ = unstructured.SetNestedField(content.Object, volumeMode, "status", "dataRef", "volumeMode")
	}
	return content
}

func newResolverReconciler(objs ...runtime.Object) *DataexportReconciler {
	gvrToListKind := map[schema.GroupVersionResource]string{
		volumeSnapshotLeafGVR:   "VolumeSnapshotList",
		snapshotContentGVR:      "SnapshotContentList",
		volumeRestoreRequestGVR: "VolumeRestoreRequestList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrToListKind, objs...)
	return &DataexportReconciler{
		Dynamic:    dyn,
		RESTMapper: testRESTMapper(),
		Config:     &config.Options{ControllerNamespace: testControllerNamespace},
	}
}

func newSnapshotDataExport(group, kind, name string) *dev1alpha1.DataExport {
	return &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{Name: "de1", Namespace: "test-ns"},
		Spec: dev1alpha1.DataExportSpec{
			TargetRef: dev1alpha1.DataExportTargetRefSpec{Group: group, Kind: kind, Name: name},
		},
	}
}

func TestResolveSnapshotDataArtifact_HappyPath(t *testing.T) {
	leaf := newSnapshotLeaf("content1")
	content := newSnapshotContent("content1", "test-ns", artifactKindVolumeSnapshotContent, "vsc1", "Filesystem")
	r := newResolverReconciler(leaf, content)

	art, err := r.resolveSnapshotDataArtifact(context.Background(), newSnapshotDataExport("snapshot.storage.k8s.io", "VolumeSnapshot", "leaf1"))
	require.NoError(t, err)
	require.NotNil(t, art)
	assert.Equal(t, artifactKindVolumeSnapshotContent, art.ArtifactKind)
	assert.Equal(t, "vsc1", art.ArtifactName)
	assert.Equal(t, "Filesystem", art.VolumeMode)
}

func TestResolveSnapshotDataArtifact_Errors(t *testing.T) {
	tests := []struct {
		name         string
		leaf         *unstructured.Unstructured
		content      *unstructured.Unstructured
		targetGroup  string
		targetKind   string
		targetName   string
		wantSentinel error
	}{
		{
			name:         "missing leaf is target-not-found",
			targetGroup:  "snapshot.storage.k8s.io",
			targetKind:   "VolumeSnapshot",
			targetName:   "does-not-exist",
			wantSentinel: ErrTargetNotFound,
		},
		{
			name:         "unbound leaf is target-not-ready",
			leaf:         newSnapshotLeaf(""),
			targetGroup:  "snapshot.storage.k8s.io",
			targetKind:   "VolumeSnapshot",
			targetName:   "leaf1",
			wantSentinel: ErrTargetNotReady,
		},
		{
			name:         "content in another namespace is rejected",
			leaf:         newSnapshotLeaf("content-xns"),
			content:      newSnapshotContent("content-xns", "victim-ns", artifactKindVolumeSnapshotContent, "vsc1", "Filesystem"),
			targetGroup:  "snapshot.storage.k8s.io",
			targetKind:   "VolumeSnapshot",
			targetName:   "leaf1",
			wantSentinel: ErrTargetNotFound,
		},
		{
			name:         "dataRef without volumeMode is not-ready",
			leaf:         newSnapshotLeaf("content1"),
			content:      newSnapshotContent("content1", "test-ns", artifactKindVolumeSnapshotContent, "vsc1", ""),
			targetGroup:  "snapshot.storage.k8s.io",
			targetKind:   "VolumeSnapshot",
			targetName:   "leaf1",
			wantSentinel: ErrTargetNotReady,
		},
		{
			name:         "non-exportable artifact kind is target-not-found",
			leaf:         newSnapshotLeaf("content1"),
			content:      newSnapshotContent("content1", "test-ns", "ConfigMap", "cm1", "Filesystem"),
			targetGroup:  "snapshot.storage.k8s.io",
			targetKind:   "VolumeSnapshot",
			targetName:   "leaf1",
			wantSentinel: ErrTargetNotFound,
		},
		{
			name:         "no API mapping is target-not-found",
			targetGroup:  "nope.example.io",
			targetKind:   "Nothing",
			targetName:   "x",
			wantSentinel: ErrTargetNotFound,
		},
		{
			name:         "cluster-scoped target is rejected",
			targetGroup:  "example.io",
			targetKind:   "ClusterThing",
			targetName:   "x",
			wantSentinel: ErrTargetNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []runtime.Object
			if tt.leaf != nil {
				objs = append(objs, tt.leaf)
			}
			if tt.content != nil {
				objs = append(objs, tt.content)
			}
			r := newResolverReconciler(objs...)

			_, err := r.resolveSnapshotDataArtifact(context.Background(), newSnapshotDataExport(tt.targetGroup, tt.targetKind, tt.targetName))
			require.Error(t, err)
			if tt.wantSentinel != nil {
				assert.True(t, errors.Is(err, tt.wantSentinel), "want sentinel %v, got %v", tt.wantSentinel, err)
			}
		})
	}
}

func TestEnsureVolumeRestoreRequest_CreatesAndIsIdempotent(t *testing.T) {
	r := newResolverReconciler()
	de := newSnapshotDataExport("snapshot.storage.k8s.io", "VolumeSnapshot", "leaf1")
	names := common.NewNamesFromShort(dev1alpha1.KindSnapshotShort, "leaf1", de.Namespace, de.Name)
	art := &snapshotDataArtifact{
		ArtifactKind: artifactKindVolumeSnapshotContent,
		ArtifactName: "vsc1",
		VolumeMode:   "Block",
	}

	require.NoError(t, r.ensureVolumeRestoreRequest(context.Background(), de, names, art))

	vrr, err := r.Dynamic.Resource(volumeRestoreRequestGVR).Namespace(testControllerNamespace).Get(context.Background(), volumeRestoreRequestName(names), metav1.GetOptions{})
	require.NoError(t, err)

	sourceKind, _, _ := unstructured.NestedString(vrr.Object, "spec", "sourceRef", "kind")
	sourceName, _, _ := unstructured.NestedString(vrr.Object, "spec", "sourceRef", "name")
	targetPVC, _, _ := unstructured.NestedString(vrr.Object, "spec", "pvcTemplate", "metadata", "name")
	_, hasTargetNS, _ := unstructured.NestedString(vrr.Object, "spec", "pvcTemplate", "metadata", "namespace")
	volumeMode, _, _ := unstructured.NestedString(vrr.Object, "spec", "pvcTemplate", "spec", "volumeMode")
	_, hasLegacyTargetRef, _ := unstructured.NestedMap(vrr.Object, "spec", "targetRef")
	metaNS := vrr.GetNamespace()
	assert.Equal(t, artifactKindVolumeSnapshotContent, sourceKind)
	assert.Equal(t, "vsc1", sourceName)
	assert.False(t, hasLegacyTargetRef, "spec.targetRef must not be set (replaced by pvcTemplate)")
	assert.Equal(t, names.ExportPVCName, targetPVC)
	// Restore is never cross-namespace: pvcTemplate.metadata carries no namespace; the target lives in
	// the VRR namespace.
	assert.False(t, hasTargetNS, "spec.pvcTemplate.metadata.namespace must not be set")
	assert.Equal(t, testControllerNamespace, metaNS)
	assert.Equal(t, "Block", volumeMode)

	// Second call must be a no-op (Get-before-Create), not an error.
	require.NoError(t, r.ensureVolumeRestoreRequest(context.Background(), de, names, art))
}

func TestDeleteVolumeRestoreRequest_Idempotent(t *testing.T) {
	r := newResolverReconciler()
	de := newSnapshotDataExport("snapshot.storage.k8s.io", "VolumeSnapshot", "leaf1")
	names := common.NewNamesFromShort(dev1alpha1.KindSnapshotShort, "leaf1", de.Namespace, de.Name)

	// Deleting a non-existent VRR is success.
	require.NoError(t, r.deleteVolumeRestoreRequest(context.Background(), de, names))

	art := &snapshotDataArtifact{ArtifactKind: artifactKindVolumeSnapshotContent, ArtifactName: "vsc1", VolumeMode: "Filesystem"}
	require.NoError(t, r.ensureVolumeRestoreRequest(context.Background(), de, names, art))
	require.NoError(t, r.deleteVolumeRestoreRequest(context.Background(), de, names))

	_, err := r.Dynamic.Resource(volumeRestoreRequestGVR).Namespace(testControllerNamespace).Get(context.Background(), volumeRestoreRequestName(names), metav1.GetOptions{})
	require.Error(t, err)
}
