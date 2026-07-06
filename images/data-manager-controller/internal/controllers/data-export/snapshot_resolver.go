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
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// targetCategory classifies a DataExport target by its GroupKind so the controller can dispatch
// without compiling in domain types: live PVC and live VirtualDisk keep their bespoke direct-export
// paths; everything else is a snapshot leaf exported through the resource-agnostic SnapshotContent ->
// VolumeRestoreRequest path (C6).
type targetCategory int

const (
	categoryLivePVC targetCategory = iota
	categoryLiveVirtualDisk
	categorySnapshot
)

const (
	virtualDisksGroup    = dev1alpha1.GroupVirtualization
	virtualDiskKind      = dev1alpha1.KindVirtualDisk
	snapshotStorageGroup = dev1alpha1.GroupSnapshotStorage

	// artifact kinds the SnapshotContent.dataRef may point at (the durable data leg). A VRR accepts
	// either as its sourceRef. artifactKindVolumeSnapshotContent doubles as the forbidden bare-target kind.
	artifactKindVolumeSnapshotContent = dev1alpha1.KindVolumeSnapshotContent
	artifactKindPersistentVolume      = "PersistentVolume"

	// snapshotExportPollInterval/snapshotExportPollTimeout bound how long a single reconcile waits for
	// the export PVC the VolumeRestoreRequest provisions out-of-band (via the patched external-provisioner).
	// The window is deliberately short so the (single) reconcile worker is not parked for minutes: the VRR
	// is created idempotently before polling, so timing out just yields the worker and the next requeue
	// re-polls without redoing work.
	snapshotExportPollInterval = 2 * time.Second
	snapshotExportPollTimeout  = 10 * time.Second
)

var (
	snapshotContentGVR = schema.GroupVersionResource{
		// SnapshotContent is owned by state-snapshotter (cross-module read), so it keeps the
		// state-snapshotter API group, not storage-foundation's.
		Group:    "state-snapshotter.deckhouse.io",
		Version:  "v1alpha1",
		Resource: "snapshotcontents",
	}
	volumeRestoreRequestGVR = schema.GroupVersionResource{
		Group:    "storage-foundation.deckhouse.io",
		Version:  "v1alpha1",
		Resource: "volumerestorerequests",
	}
)

const volumeRestoreRequestKind = "VolumeRestoreRequest"

// classifyTargetRef maps a GroupKind targetRef to a dispatch category and the stable short kind used
// for deterministic resource naming. A bare cluster-scoped VolumeSnapshotContent is rejected (it has no
// tenant boundary -> cross-namespace exfiltration); structural emptiness is also rejected here so both
// the controller and the (structural) admission webhook share one source of truth.
func classifyTargetRef(group, kind string) (targetCategory, string, error) {
	switch {
	case kind == "":
		return 0, "", fmt.Errorf("spec.targetRef.kind is empty")
	case group == "" && kind == dev1alpha1.KindPVC:
		return categoryLivePVC, dev1alpha1.KindPVCShort, nil
	case group == virtualDisksGroup && kind == virtualDiskKind:
		return categoryLiveVirtualDisk, dev1alpha1.KindVirtualDiskShort, nil
	case group == snapshotStorageGroup && kind == artifactKindVolumeSnapshotContent:
		return 0, "", fmt.Errorf("spec.targetRef must not reference a bare cluster-scoped VolumeSnapshotContent; reference a namespaced snapshot resource instead")
	default:
		return categorySnapshot, dev1alpha1.KindSnapshotShort, nil
	}
}

// snapshotDataArtifact is the trusted view of a snapshot leaf's data leg, read from the leaf's bound
// SnapshotContent.status.dataRef. Every field originates from the controller-written SnapshotContent
// (never from user input or annotations) — this is the C6 trust hardening: the artifact name and the
// restore parameters are taken from the snapshot, not the request.
type snapshotDataArtifact struct {
	ArtifactKind     string
	ArtifactName     string
	VolumeMode       string
	StorageClassName string
	FsType           string
	AccessModes      []string
}

// resolveSnapshotDataArtifact walks targetRef -> leaf.status.boundSnapshotContentName -> SnapshotContent
// -> status.dataRef.artifact for any registered snapshot kind via the dynamic client (no domain types
// compiled in). A not-yet-bound leaf / not-yet-populated dataRef is a transient ErrTargetNotReady (the
// snapshot is still being captured or imported), distinct from a genuinely missing leaf
// (ErrTargetNotFound).
func (r *DataexportReconciler) resolveSnapshotDataArtifact(ctx context.Context, dataExport *dev1alpha1.DataExport) (*snapshotDataArtifact, error) {
	tr := dataExport.Spec.TargetRef

	// Resolve {group, kind} -> served version + plural resource via the RESTMapper (the version is not
	// pinned in the targetRef). The same mapping carries the resource scope used by the namespaced-only
	// security guard below.
	mapping, err := r.RESTMapper.RESTMapping(schema.GroupKind{Group: tr.Group, Kind: tr.Kind})
	if err != nil {
		// A typo'd kind / not-yet-installed CRD has no REST mapping. Surface it as a
		// (visible, retried) missing target rather than an opaque internal error.
		if meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("target %s/%s has no API mapping (CRD not installed?): %w", tr.Group, tr.Kind, ErrTargetNotFound)
		}
		return nil, fmt.Errorf("resolve target %s/%s: %w", tr.Group, tr.Kind, err)
	}
	gvr := mapping.Resource

	// Security: the snapshot export path is namespaced-only. Reject any target that resolves to a
	// cluster-scoped resource generically (covers a bare VolumeSnapshotContent, SnapshotContent,
	// persistentvolumes, ...) instead of relying on a string allowlist or on the API server 404'ing a
	// namespaced GET against a cluster-scoped resource.
	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		// Terminal: a cluster-scoped target can never be a namespaced snapshot leaf. Surface it.
		return nil, fmt.Errorf("spec.targetRef %s resolves to a cluster-scoped resource; only namespaced snapshot leaves may be exported: %w", gvr.String(), ErrTargetNotFound)
	}

	leaf, err := r.Dynamic.Resource(gvr).Namespace(dataExport.Namespace).Get(ctx, tr.Name, metav1.GetOptions{})
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, fmt.Errorf("snapshot leaf %s %s/%s not found: %w", gvr.String(), dataExport.Namespace, tr.Name, ErrTargetNotFound)
		}
		return nil, fmt.Errorf("get snapshot leaf %s %s/%s: %w", gvr.String(), dataExport.Namespace, tr.Name, err)
	}

	contentName, _, err := unstructured.NestedString(leaf.Object, "status", "boundSnapshotContentName")
	if err != nil {
		return nil, fmt.Errorf("read status.boundSnapshotContentName on %s %s/%s: %w", gvr.String(), dataExport.Namespace, tr.Name, err)
	}
	if contentName == "" {
		return nil, fmt.Errorf("snapshot leaf %s %s/%s is not bound to a SnapshotContent yet: %w", gvr.String(), dataExport.Namespace, tr.Name, ErrTargetNotReady)
	}

	content, err := r.Dynamic.Resource(snapshotContentGVR).Get(ctx, contentName, metav1.GetOptions{})
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, fmt.Errorf("SnapshotContent %s referenced by %s/%s not found yet: %w", contentName, dataExport.Namespace, tr.Name, ErrTargetNotReady)
		}
		return nil, fmt.Errorf("get SnapshotContent %s: %w", contentName, err)
	}

	// Security (tenant isolation): SnapshotContent is cluster-scoped, but the binding pointer
	// (leaf.status.boundSnapshotContentName) is read from a namespaced leaf the caller can influence.
	// Verify the resolved content is anchored to the DataExport's own namespace via a controller-written,
	// immutable back-reference before trusting its data artifact, so a forged leaf pointing at another
	// tenant's SnapshotContent cannot exfiltrate its volume. The anchor is spec.snapshotRef.namespace
	// (owning Snapshot, set at creation incl. import) with a fallback to status.dataRef.target.namespace
	// (captured PVC).
	if err := verifySnapshotContentNamespace(content, contentName, dataExport.Namespace); err != nil {
		return nil, err
	}

	dataRef, found, err := unstructured.NestedMap(content.Object, "status", "dataRef")
	if err != nil {
		return nil, fmt.Errorf("read status.dataRef on SnapshotContent %s: %w", contentName, err)
	}
	if !found || dataRef == nil {
		return nil, fmt.Errorf("SnapshotContent %s has no status.dataRef (no data leg) yet: %w", contentName, ErrTargetNotReady)
	}

	artifactKind, _, _ := unstructured.NestedString(dataRef, "artifact", "kind")
	artifactName, _, _ := unstructured.NestedString(dataRef, "artifact", "name")
	if artifactName == "" {
		return nil, fmt.Errorf("SnapshotContent %s dataRef has no artifact name yet: %w", contentName, ErrTargetNotReady)
	}
	if artifactKind != artifactKindVolumeSnapshotContent && artifactKind != artifactKindPersistentVolume {
		// Terminal (the data leg is recorded but not of an exportable kind): map to a visible
		// Ready=False reason rather than letting it fall through to a silent infinite requeue.
		return nil, fmt.Errorf("SnapshotContent %s dataRef artifact kind %q is not exportable (want %s or %s): %w",
			contentName, artifactKind, artifactKindVolumeSnapshotContent, artifactKindPersistentVolume, ErrTargetNotFound)
	}

	art := &snapshotDataArtifact{ArtifactKind: artifactKind, ArtifactName: artifactName}
	// volumeMode is a controller-written, required-on-populated dataRef field. An empty value means the
	// dataRef is not fully populated yet (transient), not "default to Filesystem" — guessing would
	// restore a Block source as a filesystem and serve garbage. Treat empty as not-ready.
	art.VolumeMode, _, _ = unstructured.NestedString(dataRef, "volumeMode")
	if art.VolumeMode == "" {
		return nil, fmt.Errorf("SnapshotContent %s dataRef has no volumeMode yet: %w", contentName, ErrTargetNotReady)
	}
	art.StorageClassName, _, _ = unstructured.NestedString(dataRef, "storageClassName")
	art.FsType, _, _ = unstructured.NestedString(dataRef, "fsType")
	if modes, found, _ := unstructured.NestedStringSlice(dataRef, "accessModes"); found {
		art.AccessModes = modes
	}
	return art, nil
}

// verifySnapshotContentNamespace enforces that the cluster-scoped SnapshotContent belongs to the given
// namespace via its controller-written back-references. A present anchor that disagrees is a hard reject
// (cross-namespace exfiltration attempt); when no anchor is recorded the check cannot run and the content
// is accepted (the binding still had to come from a leaf in the caller's own namespace).
func verifySnapshotContentNamespace(content *unstructured.Unstructured, contentName, wantNamespace string) error {
	anchorNS, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "namespace")
	if anchorNS == "" {
		anchorNS, _, _ = unstructured.NestedString(content.Object, "status", "dataRef", "target", "namespace")
	}
	if anchorNS != "" && anchorNS != wantNamespace {
		return fmt.Errorf("SnapshotContent %s belongs to namespace %q, not %q: target does not belong to this namespace: %w",
			contentName, anchorNS, wantNamespace, ErrTargetNotFound)
	}
	return nil
}

// getExportPVCFromSnapshot is the resource-agnostic snapshot export path: it resolves the trusted data
// artifact, ensures a VolumeRestoreRequest that restores it into the export PVC, and waits for the
// patched external-provisioner to create+bind that PVC. The export PVC is then handed to the common
// exporter-deployment machinery exactly like the live paths.
func (r *DataexportReconciler) getExportPVCFromSnapshot(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames common.Names) (*corev1.PersistentVolumeClaim, error) {
	art, err := r.resolveSnapshotDataArtifact(ctx, dataExport)
	if err != nil {
		return nil, err
	}

	if err := r.ensureVolumeRestoreRequest(ctx, dataExport, generatedNames, art); err != nil {
		return nil, err
	}

	return r.waitForExportPVC(ctx, generatedNames.ExportPVCName)
}

// volumeRestoreRequestName is the deterministic per-DataExport VRR name (keyed to the export hash so it
// is idempotent across reconciles and unique per DataExport).
func volumeRestoreRequestName(generatedNames common.Names) string {
	return "data-export-vrr-" + generatedNames.TargetKindShort + "-" + generatedNames.HashSuffix
}

// ensureVolumeRestoreRequest idempotently creates the VRR that restores the trusted artifact into the
// export PVC. volumeMode is required by the VRR schema and is taken verbatim from the trusted dataRef
// (resolveSnapshotDataArtifact guarantees it is non-empty). The VRR is created in the controller
// namespace (where the export PVC lives) and is labeled so teardown/list can find it.
func (r *DataexportReconciler) ensureVolumeRestoreRequest(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames common.Names, art *snapshotDataArtifact) error {
	name := volumeRestoreRequestName(generatedNames)
	cli := r.Dynamic.Resource(volumeRestoreRequestGVR).Namespace(r.Config.ControllerNamespace)

	if _, err := cli.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !kubeerrors.IsNotFound(err) {
		return fmt.Errorf("get VolumeRestoreRequest %s: %w", name, err)
	}

	// art.VolumeMode is guaranteed non-empty (resolveSnapshotDataArtifact rejects an empty one as
	// not-ready), so the required pvcTemplate volumeMode comes straight from the trusted dataRef.
	// pvcTemplate describes the export PVC the restore creates; its namespace is implicit = the VRR
	// namespace (restore is never cross-namespace), so metadata.namespace = ControllerNamespace applies.
	pvcSpec := map[string]interface{}{
		"volumeMode": art.VolumeMode,
	}
	if art.StorageClassName != "" {
		pvcSpec["storageClassName"] = art.StorageClassName
	}
	if len(art.AccessModes) > 0 {
		modes := make([]interface{}, 0, len(art.AccessModes))
		for _, m := range art.AccessModes {
			modes = append(modes, m)
		}
		pvcSpec["accessModes"] = modes
	}
	spec := map[string]interface{}{
		"sourceRef": map[string]interface{}{
			"kind": art.ArtifactKind,
			"name": art.ArtifactName,
		},
		"pvcTemplate": map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": generatedNames.ExportPVCName,
			},
			"spec": pvcSpec,
		},
	}
	// fsType is a restore execution parameter read by the external-provisioner, not a PVC field, so it
	// stays at spec root (optional, ignored for Block volumes).
	if art.FsType != "" {
		spec["fsType"] = art.FsType
	}

	vrr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": volumeRestoreRequestGVR.Group + "/" + volumeRestoreRequestGVR.Version,
		"kind":       volumeRestoreRequestKind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": r.Config.ControllerNamespace,
			"labels": map[string]interface{}{
				dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue,
			},
			"annotations": map[string]interface{}{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: dataExport.Namespace,
				dev1alpha1.AnnotationStorageManagerNameKey:      dataExport.Name,
			},
		},
		"spec": spec,
	}}

	if _, err := cli.Create(ctx, vrr, metav1.CreateOptions{}); err != nil && !kubeerrors.IsAlreadyExists(err) {
		return fmt.Errorf("create VolumeRestoreRequest %s: %w", name, err)
	}
	log.Printf("Created VolumeRestoreRequest %s for snapshot export of DataExport %s/%s (artifact %s/%s)",
		name, dataExport.Namespace, dataExport.Name, art.ArtifactKind, art.ArtifactName)
	return nil
}

// deleteVolumeRestoreRequest removes the export's VolumeRestoreRequest on teardown (idempotent;
// not-found is success). The VRR also self-expires by its own TTL, so this is best-effort cleanup.
func (r *DataexportReconciler) deleteVolumeRestoreRequest(ctx context.Context, _ *dev1alpha1.DataExport, generatedNames common.Names) error {
	name := volumeRestoreRequestName(generatedNames)
	err := r.Dynamic.Resource(volumeRestoreRequestGVR).Namespace(r.Config.ControllerNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !kubeerrors.IsNotFound(err) {
		return fmt.Errorf("delete VolumeRestoreRequest %s: %w", name, err)
	}
	return nil
}

// waitForExportPVC waits for the export PVC the VRR provisions to exist and bind. If it never binds
// within the poll window it returns ErrTargetNotReady so the reconcile requeues (the VRR is already
// created, so the next pass just re-polls). A PVC that exists with a VolumeMode but is not yet Bound is
// still returned (the exporter pod tolerates a pending bind), so progress is not blocked indefinitely.
func (r *DataexportReconciler) waitForExportPVC(ctx context.Context, exportPVCName string) (*corev1.PersistentVolumeClaim, error) {
	key := types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: exportPVCName}
	exportPVC := &corev1.PersistentVolumeClaim{}

	pollErr := wait.PollUntilContextTimeout(ctx, snapshotExportPollInterval, snapshotExportPollTimeout, true, func(ctx context.Context) (bool, error) {
		if err := r.Client.Get(ctx, key, exportPVC); err != nil {
			if kubeerrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if exportPVC.Spec.VolumeMode == nil {
			return false, nil
		}
		return exportPVC.Status.Phase == corev1.ClaimBound, nil
	})
	if pollErr == nil {
		return exportPVC, nil
	}

	// Timed out waiting for Bound: if the PVC at least exists and is shaped (VolumeMode set), hand it on
	// so the exporter deployment can be created while the bind completes; otherwise hold pending.
	if err := r.Client.Get(ctx, key, exportPVC); err == nil && exportPVC.Spec.VolumeMode != nil {
		return exportPVC, nil
	}
	return nil, fmt.Errorf("export PVC %s not provisioned by VolumeRestoreRequest yet: %w", exportPVCName, ErrTargetNotReady)
}
