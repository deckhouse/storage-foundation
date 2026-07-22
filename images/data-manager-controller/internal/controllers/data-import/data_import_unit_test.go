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

package dataimport

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

func TestScratchVolumeParamsFromSpec(t *testing.T) {
	svt := func(sc, size, mode string) dev1alpha1.DataImportSpec {
		return dev1alpha1.DataImportSpec{StorageParams: &dev1alpha1.StorageParamsSpec{
			StorageClassName: sc, Size: size, VolumeMode: mode,
		}}
	}

	params, err := scratchVolumeParamsFromSpec(svt("fast", "10Gi", "Block"))
	require.NoError(t, err)
	assert.Equal(t, "fast", params.StorageClassName)
	assert.Equal(t, "Block", params.VolumeMode)
	assert.Equal(t, "10Gi", params.Size.String())

	// volumeMode is optional.
	params, err = scratchVolumeParamsFromSpec(svt("fast", "3Gi", ""))
	require.NoError(t, err)
	assert.Equal(t, "", params.VolumeMode)
	assert.Equal(t, "3Gi", params.Size.String())

	// the scratch template, its storageClass and its size are required, and size must parse.
	errCases := map[string]dev1alpha1.DataImportSpec{
		"no template":     {},
		"no storageclass": svt("", "3Gi", ""),
		"no size":         svt("fast", "", ""),
		"bad size":        svt("fast", "not-a-quantity", ""),
	}
	for name, spec := range errCases {
		t.Run(name, func(t *testing.T) {
			_, err := scratchVolumeParamsFromSpec(spec)
			assert.Error(t, err)
		})
	}
}

func TestScratchPVCTemplate(t *testing.T) {
	params := scratchVolumeParams{StorageClassName: "fast", VolumeMode: "Block"}
	params.Size.Set(1024)
	tpl := scratchPVCTemplate("imp-1", params)
	assert.Equal(t, "imp-1", tpl.Name)
	require.NotNil(t, tpl.StorageClassName)
	assert.Equal(t, "fast", *tpl.StorageClassName)
	require.NotNil(t, tpl.VolumeMode)
	assert.Equal(t, dev1alpha1.PersistentVolumeBlock, *tpl.VolumeMode)
	assert.Equal(t, []dev1alpha1.PersistentVolumeAccessMode{dev1alpha1.ReadWriteOnce}, tpl.AccessModes)

	// Defaults volumeMode to Filesystem when the spec omitted it.
	params2 := scratchVolumeParams{StorageClassName: "fast"}
	tpl2 := scratchPVCTemplate("imp-2", params2)
	require.NotNil(t, tpl2.VolumeMode)
	assert.Equal(t, dev1alpha1.PersistentVolumeFilesystem, *tpl2.VolumeMode)
}

func TestResolveSnapshotCaptureMode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, storagev1.SchemeBuilder.AddToScheme(scheme))
	require.NoError(t, snapv1.SchemeBuilder.AddToScheme(scheme))

	sc := func(name, provisioner string, ann map[string]string) *storagev1.StorageClass {
		return &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: name, Annotations: ann},
			Provisioner: provisioner,
		}
	}
	vsc := func(name, driver string) *snapv1.VolumeSnapshotClass {
		return &snapv1.VolumeSnapshotClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Driver:     driver,
		}
	}
	build := func(objs ...runtime.Object) *DataImportReconciler {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		return &DataImportReconciler{Client: fakeClient}
	}
	specSC := func(name string) dev1alpha1.DataImportSpec {
		return dev1alpha1.DataImportSpec{
			Mode:          dev1alpha1.DataImportModePopulateData,
			StorageParams: &dev1alpha1.StorageParamsSpec{StorageClassName: name, Size: "1Gi"},
		}
	}

	t.Run("snapshot capable", func(t *testing.T) {
		r := build(
			sc("fast", "csi.example.com", map[string]string{storageClassVSCAnnotation: "fast-vsc"}),
			vsc("fast-vsc", "csi.example.com"),
		)
		r.dataImport = &dev1alpha1.DataImport{Spec: specSC("fast")}
		mode, kind, err := r.resolveSnapshotCaptureMode(context.Background())
		require.NoError(t, err)
		assert.Equal(t, vcrModeSnapshot, mode)
		assert.Equal(t, artifactKindVolumeSnapshotContent, kind)
	})

	t.Run("storage class not found fails closed", func(t *testing.T) {
		r := build()
		r.dataImport = &dev1alpha1.DataImport{Spec: specSC("missing")}
		_, _, err := r.resolveSnapshotCaptureMode(context.Background())
		assert.Error(t, err)
	})

	t.Run("missing annotation fails closed", func(t *testing.T) {
		r := build(sc("plain", "csi.example.com", nil))
		r.dataImport = &dev1alpha1.DataImport{Spec: specSC("plain")}
		_, _, err := r.resolveSnapshotCaptureMode(context.Background())
		assert.Error(t, err)
	})

	t.Run("referenced VolumeSnapshotClass not found fails closed", func(t *testing.T) {
		r := build(sc("fast", "csi.example.com", map[string]string{storageClassVSCAnnotation: "ghost-vsc"}))
		r.dataImport = &dev1alpha1.DataImport{Spec: specSC("fast")}
		_, _, err := r.resolveSnapshotCaptureMode(context.Background())
		assert.Error(t, err)
	})

	t.Run("driver mismatch fails closed", func(t *testing.T) {
		r := build(
			sc("fast", "csi.example.com", map[string]string{storageClassVSCAnnotation: "other-vsc"}),
			vsc("other-vsc", "csi.other.com"),
		)
		r.dataImport = &dev1alpha1.DataImport{Spec: specSC("fast")}
		_, _, err := r.resolveSnapshotCaptureMode(context.Background())
		assert.Error(t, err)
	})

	t.Run("missing storageParams fails closed", func(t *testing.T) {
		r := build()
		r.dataImport = &dev1alpha1.DataImport{Spec: dev1alpha1.DataImportSpec{Mode: dev1alpha1.DataImportModePopulateData}}
		_, _, err := r.resolveSnapshotCaptureMode(context.Background())
		assert.Error(t, err)
	})
}

func TestVolumeCaptureRequestReadyAndFailed(t *testing.T) {
	ready := vcrWithReady(string(metav1.ConditionTrue), vcrConditionReasonCompleted)
	assert.True(t, volumeCaptureRequestReady(ready))
	failed, _ := volumeCaptureRequestFailed(ready)
	assert.False(t, failed)

	pending := vcrWithReady(string(metav1.ConditionFalse), vcrConditionReasonTargetsPend)
	assert.False(t, volumeCaptureRequestReady(pending))
	failed, _ = volumeCaptureRequestFailed(pending)
	assert.False(t, failed, "TargetsPending is non-terminal")

	hardFail := vcrWithReady(string(metav1.ConditionFalse), "InternalError")
	assert.False(t, volumeCaptureRequestReady(hardFail))
	failed, reason := volumeCaptureRequestFailed(hardFail)
	assert.True(t, failed)
	assert.Equal(t, "InternalError", reason)
}

func TestVolumeCaptureArtifact(t *testing.T) {
	vcr := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"data": map[string]interface{}{
				"artifactRef": map[string]interface{}{
					"apiVersion": "snapshot.storage.k8s.io/v1",
					"kind":       artifactKindVolumeSnapshotContent,
					"name":       "snapcontent-x",
					"uid":        "8d7c6b5a-4e3f-4a2b-9c1d-0f1e2d3c4b5a",
				},
			},
		},
	}}

	art, err := volumeCaptureArtifact(vcr, artifactKindVolumeSnapshotContent)
	require.NoError(t, err)
	assert.Equal(t, "snapcontent-x", art.Name)
	assert.Equal(t, artifactKindVolumeSnapshotContent, art.Kind)
	// The artifact uid is carried through from VCR status.data.artifactRef.uid.
	assert.Equal(t, "8d7c6b5a-4e3f-4a2b-9c1d-0f1e2d3c4b5a", art.UID)

	// Kind mismatch is rejected.
	_, err = volumeCaptureArtifact(vcr, artifactKindPersistentVolume)
	assert.Error(t, err)

	// No status.data at all errors.
	empty := &unstructured.Unstructured{Object: map[string]interface{}{"status": map[string]interface{}{}}}
	_, err = volumeCaptureArtifact(empty, artifactKindVolumeSnapshotContent)
	assert.Error(t, err)
}

func TestDeterministicNames(t *testing.T) {
	assert.Equal(t, "data-import-vcr-abc", volumeCaptureRequestName("abc"))
	assert.Equal(t, "data-import-abc", objectKeeperName("abc"))
}

func TestValidateObjectKeeperOwnership(t *testing.T) {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp-1", Namespace: "ns", UID: "di-uid"},
	}
	good := buildObjectKeeper(objectKeeperName(di.UID), di)
	good.SetUID("keeper-uid")
	require.NoError(t, validateObjectKeeperOwnership(good.GetName(), good, di))

	// followObjectRef points at a different DataImport UID -> rejected.
	other := buildObjectKeeper(objectKeeperName(di.UID), di)
	require.NoError(t, unstructured.SetNestedField(other.Object, "someone-else", "spec", "followObjectRef", "uid"))
	assert.Error(t, validateObjectKeeperOwnership(other.GetName(), other, di))

	// The keeper follows the DataImport (FollowObject -> DataImport), not the VCR.
	ref, found, _ := unstructured.NestedStringMap(good.Object, "spec", "followObjectRef")
	require.True(t, found)
	assert.Equal(t, dataImportKind, ref["kind"])
	assert.Equal(t, "di-uid", ref["uid"])
	mode, _, _ := unstructured.NestedString(good.Object, "spec", "mode")
	assert.Equal(t, objectKeeperModeFollowObject, mode)
}

func TestKeeperOwnerReferenceIsNonController(t *testing.T) {
	keeper := &unstructured.Unstructured{}
	keeper.SetName("data-import-x")
	keeper.SetUID("keeper-uid")
	ref := keeperOwnerReference(keeper)
	assert.Equal(t, objectKeeperKind, ref.Kind)
	assert.Equal(t, "data-import-x", ref.Name)
	// Must NOT be a controller owner: storage-foundation's VCR retainer is the sole controller.
	assert.Nil(t, ref.Controller)
}

func TestBuildVolumeCaptureRequestTargetsScratchPVC(t *testing.T) {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp-1", Namespace: "ns", UID: "di-uid"},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "imp-1", Namespace: "ns", UID: "pvc-uid"},
	}
	vcr := buildVolumeCaptureRequest(volumeCaptureRequestName(di.UID), di, pvc, vcrModeSnapshot)

	mode, _, _ := unstructured.NestedString(vcr.Object, "spec", "mode")
	assert.Equal(t, vcrModeSnapshot, mode)

	// Single-target VCR: spec.target is a single object (not a spec.targets[] list). The list shape is
	// pruned by the CRD and leaves the VCR target-less, so guard against regressing to it.
	_, found, err := unstructured.NestedSlice(vcr.Object, "spec", "targets")
	require.NoError(t, err)
	require.False(t, found, "spec.targets[] list must not be emitted (single-target VCR)")

	target, found, err := unstructured.NestedMap(vcr.Object, "spec", "target")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "pvc-uid", target["uid"])
	assert.Equal(t, "PersistentVolumeClaim", target["kind"])
	assert.Equal(t, "imp-1", target["name"])
	// Namespace is intentionally omitted: the captured PVC lives in the VCR's own namespace.
	_, hasNS := target["namespace"]
	assert.False(t, hasNS, "spec.target.namespace must be omitted")

	owners := vcr.GetOwnerReferences()
	require.Len(t, owners, 1)
	assert.Equal(t, dataImportKind, owners[0].Kind)
	assert.Equal(t, "imp-1", owners[0].Name)
	assert.Equal(t, di.UID, owners[0].UID)
	require.NotNil(t, owners[0].Controller)
	assert.True(t, *owners[0].Controller)
}

// TestBuildVolumeCaptureRequest_ConformsToVCRAPIType guards against cross-repo shape drift between the
// DataImport VCR writer (which talks to the VCR via unstructured maps) and the VolumeCaptureRequest API
// type / CRD. A plain map-shape assertion (see TestBuildVolumeCaptureRequestTargetsScratchPVC) is
// self-referential: it stays green even when the writer emits a field the CRD does not have (the legacy
// spec.targets[]), because it just re-reads the same hand-built map. The API server, in contrast,
// structurally prunes unknown fields, so such a writer silently produced a target-less VCR that never
// captured -- a bug only the real-API e2e caught, never a unit test.
//
// This test closes that gap by decoding the writer's output into the real dev1alpha1.VolumeCaptureRequest
// with DisallowUnknownFields: any field the API type (hence the CRD's structural schema) would prune is a
// hard failure here, at unit level. It then mirrors the CRD's mode=Snapshot => spec.target CEL rule so a
// writer that drops the required target also fails locally.
func TestBuildVolumeCaptureRequest_ConformsToVCRAPIType(t *testing.T) {
	di := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp-1", Namespace: "ns", UID: "di-uid"},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "imp-1", Namespace: "ns", UID: "pvc-uid"},
	}
	vcr := buildVolumeCaptureRequest(volumeCaptureRequestName(di.UID), di, pvc, vcrModeSnapshot)

	raw, err := json.Marshal(vcr.Object)
	require.NoError(t, err)

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var typed dev1alpha1.VolumeCaptureRequest
	require.NoError(t, dec.Decode(&typed),
		"writer emitted a field the VolumeCaptureRequest CRD would prune (e.g. legacy spec.targets[]); "+
			"it must conform to dev1alpha1.VolumeCaptureRequest")

	// Mirror the CRD CEL rule: mode=Snapshot requires spec.target carrying the captured PVC identity.
	assert.Equal(t, dev1alpha1.VolumeCaptureModeSnapshot, typed.Spec.Mode)
	require.NotNil(t, typed.Spec.Target, "spec.target is required when mode is Snapshot")
	assert.Equal(t, "pvc-uid", typed.Spec.Target.UID)
	assert.Equal(t, "v1", typed.Spec.Target.APIVersion)
	assert.Equal(t, "PersistentVolumeClaim", typed.Spec.Target.Kind)
	assert.Equal(t, "imp-1", typed.Spec.Target.Name)
}

func TestIsCreatePVCMode(t *testing.T) {
	cases := map[string]struct {
		mode dev1alpha1.DataImportMode
		want bool
	}{
		"CreatePVC":                   {mode: dev1alpha1.DataImportModeCreatePVC, want: true},
		"PopulateData":                {mode: dev1alpha1.DataImportModePopulateData, want: false},
		"empty defaults to CreatePVC": {mode: "", want: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := &DataImportReconciler{dataImport: &dev1alpha1.DataImport{
				Spec: dev1alpha1.DataImportSpec{Mode: tc.mode},
			}}
			assert.Equal(t, tc.want, r.isCreatePVCMode())
		})
	}
}

func TestEnsurePVCImportTargetRequiresTemplate(t *testing.T) {
	r := &DataImportReconciler{dataImport: &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: "imp-b", Namespace: "ns"},
		Spec:       dev1alpha1.DataImportSpec{Mode: dev1alpha1.DataImportModeCreatePVC},
	}}
	_, err := r.ensurePVCImportTarget(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetFailed)
}

// newCreatePVCReconciler builds a CreatePVC reconciler whose PVC import target is restored-pvc, with a
// fake client carrying only corev1/storagev1 (the populate path never touches snapshot machinery).
func newCreatePVCReconciler(objs ...runtime.Object) *DataImportReconciler {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = storagev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return &DataImportReconciler{
		Client: fakeClient,
		dataImport: &dev1alpha1.DataImport{
			ObjectMeta: metav1.ObjectMeta{Name: "imp-b", Namespace: "ns"},
			Spec: dev1alpha1.DataImportSpec{
				Mode: dev1alpha1.DataImportModeCreatePVC,
				PvcTemplate: &dev1alpha1.PersistentVolumeClaimTemplateSpec{
					PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: "restored-pvc"},
				},
			},
		},
	}
}

func boundPVC(volumeMode corev1.PersistentVolumeMode) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "restored-pvc", Namespace: "ns"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: &volumeMode},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func TestHandlePVCImportStatusCompletesWithoutArtifact(t *testing.T) {
	r := newCreatePVCReconciler()
	meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
		Type:   string(common.ConditionUploadFinished),
		Status: metav1.ConditionTrue,
		Reason: "UploadFinished",
	})

	res, err := r.handlePVCImportStatus(context.Background(), boundPVC(corev1.PersistentVolumeFilesystem))
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
	assert.True(t, meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)))
	// The defining difference of CreatePVC: no durable artifact is produced.
	assert.Nil(t, r.dataImport.Status.Data)
	assert.Equal(t, "Filesystem", r.dataImport.Status.VolumeMode)
}

func TestHandlePVCImportStatusAwaitsUpload(t *testing.T) {
	r := newCreatePVCReconciler()

	res, err := r.handlePVCImportStatus(context.Background(), boundPVC(corev1.PersistentVolumeFilesystem))
	require.NoError(t, err)
	assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)
	assert.False(t, meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)))
	ready := meta.FindStatusCondition(r.dataImport.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
}

func TestEnsureImportPVCWiresDataSourceRefToDataImport(t *testing.T) {
	r := newCreatePVCReconciler()

	sc := "fast"
	fs := dev1alpha1.PersistentVolumeFilesystem
	tmpl := &dev1alpha1.PersistentVolumeClaimTemplateSpec{
		PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{
			Name:   "restored-pvc",
			Labels: map[string]string{"app": "my-app"},
		},
		PersistentVolumeClaimSpec: dev1alpha1.PersistentVolumeClaimSpec{
			AccessModes:      []dev1alpha1.PersistentVolumeAccessMode{dev1alpha1.ReadWriteOnce},
			Resources:        dev1alpha1.VolumeResourceRequirements{Requests: dev1alpha1.ResourceList{dev1alpha1.ResourceStorage: resource.MustParse("5Gi")}},
			StorageClassName: &sc,
			VolumeMode:       &fs,
		},
	}
	require.NoError(t, r.ensureImportPVC(context.Background(), tmpl))

	got := &corev1.PersistentVolumeClaim{}
	require.NoError(t, r.Client.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "restored-pvc"}, got))
	require.NotNil(t, got.Spec.DataSourceRef)
	// The populator keys the upload server off DataSourceRef -> DataImport, so the user-named PVC is
	// still wired back to the import.
	assert.Equal(t, dataImportKind, got.Spec.DataSourceRef.Kind)
	assert.Equal(t, "imp-b", got.Spec.DataSourceRef.Name)
	assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName)
	assert.Equal(t, dev1alpha1.LabelDataImportValue, got.Labels[dev1alpha1.LabelApplicationKey])
}

func vcrWithReady(status, reason string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{
					"type":   vcrConditionTypeReady,
					"status": status,
					"reason": reason,
				},
			},
		},
	}}
}
