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
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	makeContent := func(snapshotRefNS, sourceNS string) *unstructured.Unstructured {
		c := &unstructured.Unstructured{Object: map[string]interface{}{}}
		if snapshotRefNS != "" {
			_ = unstructured.SetNestedField(c.Object, snapshotRefNS, "spec", "snapshotRef", "namespace")
		}
		if sourceNS != "" {
			_ = unstructured.SetNestedField(c.Object, sourceNS, "status", "data", "sourceRef", "namespace")
		}
		return c
	}

	tests := []struct {
		name          string
		snapshotRefNS string
		sourceNS      string
		wantNamespace string
		wantErr       bool
	}{
		{name: "snapshotRef namespace matches", snapshotRefNS: "test-ns", wantNamespace: "test-ns"},
		{name: "snapshotRef namespace mismatches -> rejected", snapshotRefNS: "victim-ns", wantNamespace: "test-ns", wantErr: true},
		{name: "fallback to data.sourceRef namespace matches", sourceNS: "test-ns", wantNamespace: "test-ns"},
		{name: "fallback to data.sourceRef namespace mismatches -> rejected", sourceNS: "victim-ns", wantNamespace: "test-ns", wantErr: true},
		{name: "snapshotRef takes precedence over data.sourceRef", snapshotRefNS: "test-ns", sourceNS: "victim-ns", wantNamespace: "test-ns"},
		{name: "no anchor recorded -> accepted", wantNamespace: "test-ns"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifySnapshotContentNamespace(makeContent(tt.snapshotRefNS, tt.sourceNS), "content1", tt.wantNamespace)
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
		_ = unstructured.SetNestedField(content.Object, artifactKind, "status", "data", "artifactRef", "kind")
	}
	if artifactName != "" {
		_ = unstructured.SetNestedField(content.Object, artifactName, "status", "data", "artifactRef", "name")
	}
	if volumeMode != "" {
		_ = unstructured.SetNestedField(content.Object, volumeMode, "status", "data", "volumeMode")
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
			name:         "data without volumeMode is not-ready",
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

// staleAccessModesKey is the status.data key this module used to consume. It is spelled out in the test
// only: production code must not mention it any more, so there is deliberately no constant to import.
const staleAccessModesKey = "accessModes"

// withStaleAccessModes stamps the removed status.data.accessModes key onto a SnapshotContent. It stands
// for an etcd row written before the field left the state-snapshotter schema (the value physically
// survives there until the object is rewritten) and for any writer that still emits it. Such a value
// must neither block the export nor reach the VolumeRestoreRequest.
func withStaleAccessModes(content *unstructured.Unstructured, modes ...string) *unstructured.Unstructured {
	if err := unstructured.SetNestedStringSlice(content.Object, modes, "status", "data", staleAccessModesKey); err != nil {
		panic(err)
	}
	return content
}

// findKeyPaths walks every map key of a nested unstructured object and returns how many keys it visited
// plus the paths whose key equals want. Limit: it descends maps and slices only (a VRR object holds
// nothing else) and it matches key names, not values.
func findKeyPaths(obj interface{}, want, path string) (visited int, hits []string) {
	switch typed := obj.(type) {
	case map[string]interface{}:
		for key, value := range typed {
			childPath := path + "." + key
			visited++
			if key == want {
				hits = append(hits, childPath)
			}
			childVisited, childHits := findKeyPaths(value, want, childPath)
			visited += childVisited
			hits = append(hits, childHits...)
		}
	case []interface{}:
		for i, value := range typed {
			childVisited, childHits := findKeyPaths(value, want, fmt.Sprintf("%s[%d]", path, i))
			visited += childVisited
			hits = append(hits, childHits...)
		}
	}
	return visited, hits
}

// TestSnapshotDataArtifact_CarriesNoAccessModes pins the removal at the type level: the trusted view of
// status.data has no access-mode field, so nothing can read one back in without tripping this test.
// Limit: it matches field names, so a differently named field holding access modes would pass here and
// be caught instead by the VRR-shape tests below.
func TestSnapshotDataArtifact_CarriesNoAccessModes(t *testing.T) {
	artType := reflect.TypeOf(snapshotDataArtifact{})
	for i := 0; i < artType.NumField(); i++ {
		name := artType.Field(i).Name
		assert.NotContains(t, strings.ToLower(name), "accessmode",
			"snapshotDataArtifact must not carry access modes: the export PVC is a single-pod transit volume and the provisioner defaults them")
	}
	t.Logf("snapshotDataArtifact fields inspected: %d", artType.NumField())
	require.NotZero(t, artType.NumField(), "reflection found no fields at all — the check would be vacuously green")
}

// TestResolveSnapshotDataArtifact_StaleAccessModesIgnored is the "former RWX volume still exports" case
// at the resolve stage: a leftover ReadWriteMany value must be ignored, not turned into a refusal, and
// the sibling fields must still be picked up.
func TestResolveSnapshotDataArtifact_StaleAccessModesIgnored(t *testing.T) {
	leaf := newSnapshotLeaf("content1")
	content := withStaleAccessModes(
		newSnapshotContent("content1", "test-ns", artifactKindVolumeSnapshotContent, "vsc1", "Filesystem"),
		"ReadWriteMany", "ReadOnlyMany",
	)
	require.NoError(t, unstructured.SetNestedField(content.Object, "local", "status", "data", "storageClassName"))
	require.NoError(t, unstructured.SetNestedField(content.Object, "ext4", "status", "data", "fsType"))
	r := newResolverReconciler(leaf, content)

	art, err := r.resolveSnapshotDataArtifact(context.Background(), newSnapshotDataExport("snapshot.storage.k8s.io", "VolumeSnapshot", "leaf1"))
	require.NoError(t, err, "a ReadWriteMany source must stay exportable")
	require.NotNil(t, art)
	assert.Equal(t, "Filesystem", art.VolumeMode)
	assert.Equal(t, "local", art.StorageClassName)
	assert.Equal(t, "ext4", art.FsType)
}

// TestEnsureVolumeRestoreRequest_OmitsAccessModes is the mutation gate for the removal: restoring the
// old read+write of status.data.accessModes makes the VRR carry the key again and fails this test. The
// export PVC template is expected to name only the fields the restore genuinely needs; access modes are
// supplied by the patched external-provisioner (effectiveAccessModes -> ReadWriteOnce).
func TestEnsureVolumeRestoreRequest_OmitsAccessModes(t *testing.T) {
	leaf := newSnapshotLeaf("content1")
	content := withStaleAccessModes(
		newSnapshotContent("content1", "test-ns", artifactKindVolumeSnapshotContent, "vsc1", "Filesystem"),
		"ReadWriteMany",
	)
	require.NoError(t, unstructured.SetNestedField(content.Object, "local", "status", "data", "storageClassName"))
	require.NoError(t, unstructured.SetNestedField(content.Object, "ext4", "status", "data", "fsType"))
	r := newResolverReconciler(leaf, content)
	de := newSnapshotDataExport("snapshot.storage.k8s.io", "VolumeSnapshot", "leaf1")
	names := common.NewNamesFromShort(dev1alpha1.KindSnapshotShort, "leaf1", de.Namespace, de.Name)

	art, err := r.resolveSnapshotDataArtifact(context.Background(), de)
	require.NoError(t, err)
	require.NoError(t, r.ensureVolumeRestoreRequest(context.Background(), de, names, art))

	vrr, err := r.Dynamic.Resource(volumeRestoreRequestGVR).Namespace(testControllerNamespace).Get(context.Background(), volumeRestoreRequestName(names), metav1.GetOptions{})
	require.NoError(t, err)

	visited, hits := findKeyPaths(vrr.Object, staleAccessModesKey, "vrr")
	t.Logf("VRR object keys inspected: %d", visited)
	require.NotZero(t, visited, "key walk found nothing — the check would be vacuously green")
	assert.Empty(t, hits, "the VolumeRestoreRequest must carry no accessModes key")

	pvcSpec, found, err := unstructured.NestedMap(vrr.Object, "spec", "pvcTemplate", "spec")
	require.NoError(t, err)
	require.True(t, found)
	specKeys := make([]string, 0, len(pvcSpec))
	for key := range pvcSpec {
		specKeys = append(specKeys, key)
	}
	// Exact set on purpose: a newly added pvcTemplate.spec field has to be declared here, which is the
	// only way this test keeps catching a re-added accessModes among other churn.
	assert.ElementsMatch(t, []string{"volumeMode", "storageClassName"}, specKeys)
	fsType, _, _ := unstructured.NestedString(vrr.Object, "spec", "fsType")
	assert.Equal(t, "ext4", fsType, "fsType stays a spec-root restore parameter, unaffected by the removal")
}

// TestGetExportPVCFromSnapshot_FormerRWXSourceExports covers the whole sf-side export path for a source
// that was ReadWriteMany: resolve -> VRR -> the export PVC the provisioner binds out of band. Dropping
// the access modes must not degrade this into a wait or an error. The export PVC is pre-seeded because
// the patched external-provisioner (not this controller) creates it.
func TestGetExportPVCFromSnapshot_FormerRWXSourceExports(t *testing.T) {
	de := newSnapshotDataExport("snapshot.storage.k8s.io", "VolumeSnapshot", "leaf1")
	names := common.NewNamesFromShort(dev1alpha1.KindSnapshotShort, "leaf1", de.Namespace, de.Name)
	filesystem := corev1.PersistentVolumeFilesystem
	exportPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: names.ExportPVCName, Namespace: testControllerNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeMode: &filesystem,
			// ReadWriteOnce: what effectiveAccessModes in the provisioner patch substitutes when the VRR
			// template names no modes, regardless of the source volume having been ReadWriteMany.
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	leaf := newSnapshotLeaf("content1")
	content := withStaleAccessModes(
		newSnapshotContent("content1", "test-ns", artifactKindVolumeSnapshotContent, "vsc1", "Filesystem"),
		"ReadWriteMany",
	)
	r := newResolverReconciler(leaf, content)
	r.Client = fake.NewClientBuilder().WithScheme(setupTestScheme()).WithObjects(exportPVC).Build()

	got, err := r.getExportPVCFromSnapshot(context.Background(), de, names)
	require.NoError(t, err, "a former ReadWriteMany source must still export")
	require.NotNil(t, got)
	assert.Equal(t, names.ExportPVCName, got.Name)

	vrr, err := r.Dynamic.Resource(volumeRestoreRequestGVR).Namespace(testControllerNamespace).Get(context.Background(), volumeRestoreRequestName(names), metav1.GetOptions{})
	require.NoError(t, err)
	_, hits := findKeyPaths(vrr.Object, staleAccessModesKey, "vrr")
	assert.Empty(t, hits, "the VolumeRestoreRequest must carry no accessModes key")
}

// TestProvisionerPatchStillDefaultsAccessModes pins the assumption this module now relies on: the
// external-provisioner patch, not the export resolver, supplies ReadWriteOnce when a VRR names no
// access modes. Losing that default would silently break every snapshot export, so the dependency is
// asserted here instead of being left as a comment. Limit: this is a text check on the patch — it
// proves the default is written, not that the patched binary runs it (that belongs to e2e).
func TestProvisionerPatchStillDefaultsAccessModes(t *testing.T) {
	const patchPath = "../../../../csi-external-provisioner/patches/v6.2.0/002-vrr-executor.patch"

	raw, err := os.ReadFile(patchPath)
	require.NoError(t, err, "the provisioner patch must stay readable: the export PVC has no access modes of its own and depends on its default")
	patch := string(raw)

	// The default lives in effectiveAccessModes; the two call sites are the CSI volume capability and the
	// PV/PVC the restore creates.
	needles := []string{
		"func effectiveAccessModes(",
		"return []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}",
		"accessmodes.ToCSIAccessMode(effectiveAccessModes(vrr)",
		"effectiveAccessModes(vrr))",
	}
	for _, needle := range needles {
		assert.Contains(t, patch, needle,
			"provisioner patch no longer defaults VRR access modes to ReadWriteOnce; the export PVC template stopped setting them on purpose")
	}
	t.Logf("provisioner patch fragments inspected: %d (patch %d bytes)", len(needles), len(patch))
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
