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
	"context"
	"fmt"
	"time"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// dataImportRequeueInterval is the resync cadence while waiting on out-of-band objects the controller
// creates but does not watch (VolumeCaptureRequest, ObjectKeeper). It is a polling fallback, not a
// deadline.
const dataImportRequeueInterval = 5 * time.Second

// dataImportKind is the DataImport kind string, used for ownerReferences on objects DataImport owns.
const dataImportKind = "DataImport"

// VolumeCaptureRequest contract (storage-foundation, talked to via unstructured to avoid a Go dep,
// mirroring state-snapshotter's approach).
const (
	volumeCaptureRequestKind = "VolumeCaptureRequest"

	vcrModeSnapshot = "Snapshot"

	vcrConditionTypeReady         = "Ready"
	vcrConditionReasonCompleted   = "Completed"
	vcrConditionReasonTargetsPend = "TargetsPending"

	artifactKindVolumeSnapshotContent = "VolumeSnapshotContent"
	artifactKindPersistentVolume      = "PersistentVolume"

	// storageClassVSCAnnotation is the StorageClass annotation that names the VolumeSnapshotClass used
	// for CSI snapshots of volumes on that class. storage-foundation's VolumeCaptureRequest controller
	// resolves the VSC through this annotation, so DataImport mirrors it to decide snapshot capability.
	storageClassVSCAnnotation = "storage.deckhouse.io/volumesnapshotclass"
)

var volumeCaptureRequestGVR = schema.GroupVersionResource{
	Group:    "storage-foundation.deckhouse.io",
	Version:  "v1alpha1",
	Resource: "volumecapturerequests",
}

// resolveSnapshotCaptureMode decides the VolumeCaptureRequest mode and the durable artifact kind for the
// import. Core supports snapshot-capable CSI only: it resolves the VolumeSnapshotClass for the import's
// StorageClass the same way storage-foundation's VCR controller does (via the StorageClass
// storage.deckhouse.io/volumesnapshotclass annotation, validating the referenced VolumeSnapshotClass
// exists) and returns mode=Snapshot -> VolumeSnapshotContent. The driver check here is against the
// StorageClass provisioner; the VCR re-validates against the bound PV's CSI driver, which equals the
// provisioner for dynamically provisioned volumes. If the StorageClass is not snapshot-capable
// (no/invalid VolumeSnapshotClass) it fails closed: PersistentVolume/Detach import is intentionally not
// supported here.
func (r *DataImportReconciler) resolveSnapshotCaptureMode(ctx context.Context) (mode, expectedKind string, err error) {
	tmpl := r.dataImport.Spec.StorageParams
	if tmpl == nil || tmpl.StorageClassName == "" {
		return "", "", fmt.Errorf("spec.storageParams.storageClassName is required to select a VolumeSnapshotClass")
	}
	scName := tmpl.StorageClassName

	sc := &storagev1.StorageClass{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: scName}, sc); err != nil {
		return "", "", fmt.Errorf("get StorageClass %q: %w", scName, err)
	}

	vscName := sc.Annotations[storageClassVSCAnnotation]
	if vscName == "" {
		return "", "", fmt.Errorf("StorageClass %q is not snapshot-capable: missing %s annotation; core import supports CSI snapshots only (PersistentVolume/Detach import is not supported)",
			scName, storageClassVSCAnnotation)
	}

	vsc := &snapv1.VolumeSnapshotClass{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: vscName}, vsc); err != nil {
		return "", "", fmt.Errorf("get VolumeSnapshotClass %q (from StorageClass %q %s annotation): %w",
			vscName, scName, storageClassVSCAnnotation, err)
	}
	if vsc.Driver != sc.Provisioner {
		return "", "", fmt.Errorf("VolumeSnapshotClass %q driver %q does not match StorageClass %q provisioner %q",
			vscName, vsc.Driver, scName, sc.Provisioner)
	}

	return vcrModeSnapshot, artifactKindVolumeSnapshotContent, nil
}

// volumeCaptureRequestName is the deterministic, per-DataImport VCR name (keyed to the DataImport UID so
// reconciles are idempotent and a recreated DataImport does not collide with a stale request).
func volumeCaptureRequestName(uid types.UID) string {
	return "data-import-vcr-" + string(uid)
}

// buildVolumeCaptureRequest constructs the VCR (unstructured) that captures the populated scratch PVC
// into the durable artifact. It is owned by the DataImport (namespaced->namespaced ownerRef) so it is
// garbage-collected with the DataImport.
func buildVolumeCaptureRequest(name string, di *dev1alpha1.DataImport, scratchPVC *corev1.PersistentVolumeClaim, mode string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": volumeCaptureRequestGVR.Group + "/" + volumeCaptureRequestGVR.Version,
		"kind":       volumeCaptureRequestKind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": di.Namespace,
			"ownerReferences": []interface{}{
				map[string]interface{}{
					"apiVersion":         dev1alpha1.SchemeGroupVersion.String(),
					"kind":               dataImportKind,
					"name":               di.Name,
					"uid":                string(di.UID),
					"controller":         true,
					"blockOwnerDeletion": true,
				},
			},
		},
		"spec": map[string]interface{}{
			"mode": mode,
			"targets": []interface{}{
				map[string]interface{}{
					"uid":        string(scratchPVC.UID),
					"apiVersion": "v1",
					"kind":       "PersistentVolumeClaim",
					"name":       scratchPVC.Name,
					"namespace":  scratchPVC.Namespace,
				},
			},
		},
	}}
}

// ensureVolumeCaptureRequest idempotently creates the VCR for this import and returns the current object.
func (r *DataImportReconciler) ensureVolumeCaptureRequest(ctx context.Context, scratchPVC *corev1.PersistentVolumeClaim, mode string) (*unstructured.Unstructured, error) {
	name := volumeCaptureRequestName(r.dataImport.UID)
	cli := r.Dynamic.Resource(volumeCaptureRequestGVR).Namespace(r.dataImport.Namespace)

	got, err := cli.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return got, nil
	}
	if !kubeerrors.IsNotFound(err) {
		return nil, fmt.Errorf("get VolumeCaptureRequest %s: %w", name, err)
	}

	obj := buildVolumeCaptureRequest(name, r.dataImport, scratchPVC, mode)
	created, err := cli.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		if kubeerrors.IsAlreadyExists(err) {
			return cli.Get(ctx, name, metav1.GetOptions{})
		}
		return nil, fmt.Errorf("create VolumeCaptureRequest %s: %w", name, err)
	}
	return created, nil
}

// volumeCaptureRequestReady reports terminal success (Ready=True, reason=Completed).
func volumeCaptureRequestReady(vcr *unstructured.Unstructured) bool {
	status, reason, ok := vcrReadyCondition(vcr)
	return ok && status == string(metav1.ConditionTrue) && reason == vcrConditionReasonCompleted
}

// volumeCaptureRequestFailed reports a terminal failure (Ready=False with a reason other than the
// non-terminal TargetsPending) and the failure reason.
func volumeCaptureRequestFailed(vcr *unstructured.Unstructured) (bool, string) {
	status, reason, ok := vcrReadyCondition(vcr)
	if ok && status == string(metav1.ConditionFalse) && reason != vcrConditionReasonTargetsPend {
		return true, reason
	}
	return false, ""
}

func vcrReadyCondition(vcr *unstructured.Unstructured) (status, reason string, ok bool) {
	conditions, found, err := unstructured.NestedSlice(vcr.Object, "status", "conditions")
	if err != nil || !found {
		return "", "", false
	}
	for _, c := range conditions {
		m, isMap := c.(map[string]interface{})
		if !isMap {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		if t != vcrConditionTypeReady {
			continue
		}
		status, _, _ = unstructured.NestedString(m, "status")
		reason, _, _ = unstructured.NestedString(m, "reason")
		return status, reason, true
	}
	return "", "", false
}

// volumeCaptureArtifact extracts the durable artifact reference from the VolumeCaptureRequest's single
// status.dataRef. wave1 collapsed the VCR to one target per request (singular status.dataRef), so there
// is no list to match by targetUID. It validates the artifact kind matches what the chosen mode must
// produce and carries the artifact uid through into DataArtifactReference.
func volumeCaptureArtifact(vcr *unstructured.Unstructured, expectedKind string) (*dev1alpha1.DataArtifactReference, error) {
	dataRef, found, err := unstructured.NestedMap(vcr.Object, "status", "data")
	if err != nil || !found || len(dataRef) == 0 {
		return nil, fmt.Errorf("VolumeCaptureRequest has no status.data")
	}

	artifact, isMap := dataRef["artifact"].(map[string]interface{})
	if !isMap {
		return nil, fmt.Errorf("VolumeCaptureRequest data has no artifact")
	}

	apiVersion, _, _ := unstructured.NestedString(artifact, "apiVersion")
	kind, _, _ := unstructured.NestedString(artifact, "kind")
	name, _, _ := unstructured.NestedString(artifact, "name")
	uid, _, _ := unstructured.NestedString(artifact, "uid")
	if kind != expectedKind {
		return nil, fmt.Errorf("VolumeCaptureRequest produced artifact kind %q, expected %q", kind, expectedKind)
	}
	if name == "" {
		return nil, fmt.Errorf("VolumeCaptureRequest artifact has empty name")
	}

	return &dev1alpha1.DataArtifactReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid}, nil
}
