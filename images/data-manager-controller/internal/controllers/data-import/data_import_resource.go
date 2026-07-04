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
	"errors"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
	"github.com/deckhouse/storage-foundation/common/publish"
)

// Sentinel errors are used as typed markers so callers can distinguish failure
// categories with errors.Is without parsing message strings.
// mutateReadyByErr maps each sentinel to the corresponding Ready condition reason.
var (
	ErrCleanupFailed = errors.New("cleanup failed")
	ErrTargetFailed  = errors.New("target failed")
)

type DataImportReconciler struct {
	Client client.Client
	Reader client.Reader
	Config *config.Options
	// Dynamic talks to CRDs SVDM has no Go types for (VolumeCaptureRequest, ObjectKeeper) without
	// compiling in their modules.
	Dynamic    dynamic.Interface
	dataImport *dev1alpha1.DataImport
	names      common.Names
}

func (r *DataImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	logger.Info("Start reconciling DataImport resource")

	dataImport := &dev1alpha1.DataImport{}
	err = r.Client.Get(ctx, req.NamespacedName, dataImport)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			logger.Error(err, "DataImport resource not found, it may have been deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get DataImport resource from cache")
		return ctrl.Result{}, err
	}

	// Copy the original state before any mutations.
	// The reconcile body mutates dataImport in-memory throughout the entire cycle
	// (conditions, finalizers, status fields, etc.) without persisting intermediate states.
	// The deferred function collects all mutations at the end:
	//   1) mutateReadyByErr — translates reconcile errors into Ready condition reasons
	//   2) updateDataImport — diffs against the original snapshot and persists only real changes
	dataImportOriginal := dataImport.DeepCopy()
	defer func() {
		mutateReadyByErr(dataImport, err)
		updateErr := updateDataImport(ctx, r.Client, dataImportOriginal, dataImport)
		err = errors.Join(err, updateErr)
	}()

	// TODO: do we need to validate DataImport?

	// Server-side resource names (upload deployment, service, ingress, dummy job) are always keyed on
	// the DataImport identity with the PVC short kind, independent of the import mode and of the actual
	// PVC name (Mode A names the scratch PVC after the DataImport; Mode B names it from pvcTemplate).
	r.names = common.NewNames(dev1alpha1.KindPVC, dataImport.Name, dataImport.Namespace, dataImport.Name)
	r.dataImport = dataImport

	if meta.FindStatusCondition(r.dataImport.Status.Conditions, string(common.ConditionReady)) == nil {
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "Started",
			ObservedGeneration: r.dataImport.Generation,
		})
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionExpired),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "TTL not expired",
			ObservedGeneration: r.dataImport.Generation,
		})
	}
	common.EnsureFinalizer(ctx, r.Client, r.dataImport, dev1alpha1.StorageManagerFinalizerName)

	if r.dataImport.DeletionTimestamp != nil {
		logger.Info("DataImport resource is marked for deletion")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonDeleted),
			Message:            "DataImport is going to be deleted",
			ObservedGeneration: r.dataImport.Generation,
		})

		if cleanupErr := r.cleanupDataImport(ctx); cleanupErr != nil {
			return ctrl.Result{}, fmt.Errorf("%w: %w", ErrCleanupFailed, cleanupErr)
		}
		return ctrl.Result{}, nil
	}

	if meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionExpired)) {
		logger.Info("DataImport resource TTL expired")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonExpired),
			Message:            "DataImport time to live expired. Please, delete this resource manually",
			ObservedGeneration: r.dataImport.Generation,
		})

		if cleanupErr := r.cleanupDataImport(ctx); cleanupErr != nil {
			return ctrl.Result{}, fmt.Errorf("%w: %w", ErrCleanupFailed, cleanupErr)
		}
		return ctrl.Result{}, nil
	}

	result, err = r.ensureTarget(ctx)
	if err != nil {
		return result, err
	}

	// When PVC is created and bound, Volume Populator controller will handle it

	if r.dataImport.Spec.Publish {
		err = r.ensurePublish(ctx)
	} else {
		_, err = r.deletePublish(ctx)
	}
	if err != nil {
		return result, err
	}

	// updateReadiness must run even while ensureTarget requeues. In the populator flow the scratch
	// PVC stays Pending for the whole upload window (it only binds after UploadFinished triggers the
	// populator's PV rebind), so ensureTarget keeps requeueing with a progress reason. The client,
	// however, waits for Ready=True (upload server up) before streaming bytes. updateReadiness is the
	// only path that flips Ready=True, and it never downgrades the more specific progress reason, so
	// calling it here both unblocks the upload and keeps the real blocking step visible.
	if err = r.updateReadiness(ctx); err != nil {
		return result, err
	}

	return result, nil
}

// mutateReadyByErr maps known reconcile errors to Ready condition reasons.
// Called in defer after the reconcile body — translates sentinel errors (e.g. ErrCleanupFailed)
// into user-visible condition reasons without persisting; the caller (updateDataImport) handles persistence.
func mutateReadyByErr(dataImport *dev1alpha1.DataImport, reconcileErr error) {
	if reconcileErr == nil || dataImport == nil {
		return
	}

	// Keep terminal status stable in TTL/deletion cleanup flows.
	// Otherwise Ready can oscillate between Expired/Deleted and CleanupFailed.
	if errors.Is(reconcileErr, ErrCleanupFailed) {
		readyCond := meta.FindStatusCondition(dataImport.Status.Conditions, string(common.ConditionReady))
		if readyCond != nil &&
			(readyCond.Reason == string(common.ReasonExpired) || readyCond.Reason == string(common.ReasonDeleted)) {
			return
		}
	}

	var reason common.ConditionReason
	switch {
	case errors.Is(reconcileErr, ErrCleanupFailed):
		reason = common.ReasonCleanupFailed
	case errors.Is(reconcileErr, ErrTargetFailed):
		reason = common.ReasonTargetFailed
	default:
		return
	}

	meta.SetStatusCondition(&dataImport.Status.Conditions, metav1.Condition{
		Type:               string(common.ConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             string(reason),
		Message:            reconcileErr.Error(),
		ObservedGeneration: dataImport.Generation,
	})
}

// isStandalonePVCImport reports whether this DataImport is a Mode B (standalone PVC import) request: the
// imported bytes are written into a user-described PVC (targetRef.pvcTemplate) that is preserved after
// the import, with no durable artifact produced. The mode is discriminated solely by
// targetRef.kind == PersistentVolumeClaim (pvcTemplate is its mandatory companion, validated by CRD CEL).
func (r *DataImportReconciler) isStandalonePVCImport() bool {
	return r.dataImport.Spec.TargetRef.Kind == dev1alpha1.KindPVC
}

// ensureTarget dispatches to the import pipeline for the active mode (selected by targetRef.kind):
// Mode B (PersistentVolumeClaim) imports into a user-described PVC; any other (snapshot leaf) kind
// captures the imported bytes into a durable VolumeSnapshotContent.
func (r *DataImportReconciler) ensureTarget(ctx context.Context) (ctrl.Result, error) {
	if r.isStandalonePVCImport() {
		return r.ensurePVCImportTarget(ctx)
	}
	return r.ensureSnapshotImportTarget(ctx)
}

// ensureSnapshotImportTarget runs the snapshot-leaf import pipeline (Mode A): derive scratch-PVC
// parameters from the spec -> ensure the scratch PVC (the volume the imported bytes land in) -> once it
// is bound, produce the durable data artifact (VolumeSnapshotContent). It returns a RequeueAfter while
// waiting on out-of-band preconditions (PVC bind, VolumeCaptureRequest completion) that the controller
// does not watch.
func (r *DataImportReconciler) ensureSnapshotImportTarget(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Terminal: once the artifact is produced the import is done. Re-affirm Completed (idempotent) and
	// stop, so completed DataImports don't re-derive the scratch PVC on every publish/server event until
	// TTL expiry.
	if r.dataImport.Status.DataArtifactRef != nil {
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:   string(common.ConditionCompleted),
			Status: metav1.ConditionTrue,
			Reason: string(common.ReasonCompleted),
			Message: fmt.Sprintf("Data import completed: produced %s %s",
				r.dataImport.Status.DataArtifactRef.Kind, r.dataImport.Status.DataArtifactRef.Name),
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{}, nil
	}

	// Scratch-PVC parameters come straight from the spec (provided by d8, mirrored from the source
	// xxxSnapshot.status); the leaf's captured manifest is no longer downloaded on import. Invalid spec
	// parameters are a terminal, user-fixable error.
	params, err := scratchVolumeParamsFromSpec(r.dataImport.Spec)
	if err != nil {
		logger.Error(err, "invalid scratch volume parameters in spec")
		return ctrl.Result{}, fmt.Errorf("%w: %w", ErrTargetFailed, err)
	}

	// Fail fast: core import supports snapshot-capable CSI only. Snapshot capability is a static
	// property of spec.storageClassName, knowable before any bytes are uploaded, so reject a
	// non-snapshot-capable StorageClass up front instead of after a full (potentially multi-GiB) upload.
	// The check is re-run at capture time (ensureDataArtifact) to guard against the annotation changing
	// mid-flight; transient API errors here are returned and recover on requeue.
	if _, _, err := r.resolveSnapshotCaptureMode(ctx); err != nil {
		logger.Error(err, "storage class is not snapshot-capable")
		return ctrl.Result{}, fmt.Errorf("%w: %w", ErrTargetFailed, err)
	}

	if err := r.ensureScratchPVC(ctx, params); err != nil {
		logger.Error(err, "failed to ensure scratch PVC")
		return ctrl.Result{}, err
	}
	pvc, err := GetScratchPVC(ctx, r.Client, r.dataImport.Namespace, r.dataImport.Name)
	if err != nil {
		logger.Error(err, "failed to get scratch PVC")
		return ctrl.Result{}, err
	}

	return r.handleTargetStatus(ctx, pvc)
}

// scratchVolumeParams holds the parameters used to size and shape the scratch PVC the imported bytes are
// written into. They are sourced from DataImportSpec (no longer from the leaf's captured manifest).
type scratchVolumeParams struct {
	StorageClassName string
	VolumeMode       string
	Size             resource.Quantity
}

// scratchVolumeParamsFromSpec validates and converts the DataImport spec volume parameters. storageClass
// and size are required (the scratch PVC cannot be built without them); volumeMode is optional and
// defaults to Filesystem downstream.
func scratchVolumeParamsFromSpec(spec dev1alpha1.DataImportSpec) (scratchVolumeParams, error) {
	if spec.StorageClassName == "" {
		return scratchVolumeParams{}, fmt.Errorf("spec.storageClassName is required")
	}
	if spec.Size == "" {
		return scratchVolumeParams{}, fmt.Errorf("spec.size is required")
	}
	size, err := resource.ParseQuantity(spec.Size)
	if err != nil {
		return scratchVolumeParams{}, fmt.Errorf("parse spec.size %q: %w", spec.Size, err)
	}
	return scratchVolumeParams{
		StorageClassName: spec.StorageClassName,
		VolumeMode:       spec.VolumeMode,
		Size:             size,
	}, nil
}

// ensureScratchPVC creates/updates the internal scratch PVC sized and shaped from the spec parameters.
// The PVC is populated by the upload path (populator + published endpoint); the produced data artifact
// is captured from it once bound.
func (r *DataImportReconciler) ensureScratchPVC(ctx context.Context, params scratchVolumeParams) error {
	return r.ensureImportPVC(ctx, scratchPVCTemplate(r.dataImport.Name, params))
}

// ensureImportPVC creates/updates the PVC the imported bytes land in from the given template, wiring its
// DataSourceRef to this DataImport so the volume populator picks it up and brings up the upload endpoint.
// Both modes share it: Mode A passes an internally-derived scratch template (named after the DataImport),
// Mode B passes the user's targetRef.pvcTemplate (named by the user).
func (r *DataImportReconciler) ensureImportPVC(ctx context.Context, pvcTemplate *dev1alpha1.PersistentVolumeClaimTemplateSpec) error {
	apiGroup := dev1alpha1.APIGroup
	dataSourceRef := &corev1.TypedObjectReference{
		APIGroup: &apiGroup,
		Kind:     dataImportKind,
		Name:     r.dataImport.Name,
	}
	resourceName := types.NamespacedName{Namespace: r.dataImport.Namespace, Name: r.dataImport.Name}
	return EnsurePVC(ctx, r.Client, resourceName, pvcTemplate, dataSourceRef)
}

// scratchPVCTemplate builds the PVC template from the spec-derived parameters: RWO access, the requested
// size, and the storageClass/volumeMode from the spec (defaulting volumeMode to Filesystem when empty).
func scratchPVCTemplate(name string, params scratchVolumeParams) *dev1alpha1.PersistentVolumeClaimTemplateSpec {
	sc := params.StorageClassName
	volumeMode := dev1alpha1.PersistentVolumeFilesystem
	if params.VolumeMode != "" {
		volumeMode = dev1alpha1.PersistentVolumeMode(params.VolumeMode)
	}
	return &dev1alpha1.PersistentVolumeClaimTemplateSpec{
		DataImportTargetRefMetaSpec: dev1alpha1.DataImportTargetRefMetaSpec{Name: name},
		PersistentVolumeClaimSpec: dev1alpha1.PersistentVolumeClaimSpec{
			AccessModes:      []dev1alpha1.PersistentVolumeAccessMode{dev1alpha1.ReadWriteOnce},
			Resources:        dev1alpha1.VolumeResourceRequirements{Requests: dev1alpha1.ResourceList{dev1alpha1.ResourceStorage: params.Size}},
			StorageClassName: &sc,
			VolumeMode:       &volumeMode,
		},
	}
}

func (r *DataImportReconciler) handleTargetStatus(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Checking scratch PVC status")
	// The scratch PVC is internal to the import (named after the DataImport, filled by the
	// populator) and never gets a natural first consumer. Under a WaitForFirstConsumer
	// StorageClass nothing would ever set volume.kubernetes.io/selected-node, so the populator
	// would wait for node selection forever. Always force a load pod for WFFC (pass
	// waitForFirstConsumer=false) so import works regardless of the StorageClass binding mode;
	// spec.WaitForFirstConsumer is intentionally not honored for this internal PVC.
	status, err := CheckPVCStatus(ctx, r.Client, pvc, false)
	if err != nil {
		logger.Error(err, "failed to check scratch PVC status")
		return ctrl.Result{}, err
	}

	volumeMode, err := GetPVCVolumeMode(pvc)
	if err != nil {
		logger.Error(err, "failed to get scratch PVC volume mode")
		return ctrl.Result{}, err
	}
	r.dataImport.Status.VolumeMode = volumeMode

	switch status {
	case TargetStatusReady:
		// PVC bound is necessary but not sufficient: the bytes must be uploaded first. The importer
		// pod flips UploadFinished=True when the client finishes streaming into the scratch PVC; only
		// then is it safe to capture the volume into the durable artifact.
		if cond := common.GetCondition(r.dataImport.Status.Conditions, common.ConditionUploadFinished); cond == nil || cond.Status != metav1.ConditionTrue {
			logger.Info("Scratch PVC is bound, awaiting data upload before capture")
			meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
				Type:               string(common.ConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             string(common.ReasonPending),
				Message:            "Scratch PVC bound, awaiting data upload",
				ObservedGeneration: r.dataImport.Generation,
			})
			return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
		}
		logger.Info("Upload finished, producing data artifact")
		return r.ensureDataArtifact(ctx, pvc)
	case TargetStatusPending:
		logger.Info("Scratch PVC is pending")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPVCCreated),
			Message:            "Scratch PVC is created",
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	case TargetStatusNeedConsumer:
		logger.Info("Scratch PVC needs consumer, ensuring dummy Job")
		if err := r.ensureDummyJob(ctx, pvc); err != nil {
			logger.Error(err, "Failed to create dummy Job")
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPVCCreated),
			Message:            "Scratch PVC is created, dummy Job created to bind PVC",
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	case TargetStatusFailed:
		return ctrl.Result{}, fmt.Errorf("scratch PVC is in Failed state: %w", ErrTargetFailed)
	}

	return ctrl.Result{}, nil
}

// ensurePVCImportTarget runs the standalone PVC import pipeline (Mode B): ensure the user-described
// target PVC (targetRef.pvcTemplate) -> once it is bound and the upload has finished, mark the import
// Completed. Unlike the snapshot-leaf path it derives nothing from the root volume params, does not gate
// on snapshot capability, and produces no durable artifact (status.dataArtifactRef stays empty); the PVC
// itself is the product and is preserved on cleanup.
func (r *DataImportReconciler) ensurePVCImportTarget(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Terminal: once Completed, stop re-ensuring the PVC on every publish/server event until TTL expiry.
	if meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)) {
		return ctrl.Result{}, nil
	}

	pvcTemplate := r.dataImport.Spec.TargetRef.PvcTemplate
	if pvcTemplate == nil || pvcTemplate.Name == "" {
		// CRD CEL enforces this; guard anyway since the controller would otherwise build an unnamed PVC.
		return ctrl.Result{}, fmt.Errorf("%w: targetRef.pvcTemplate with metadata.name is required for PersistentVolumeClaim import", ErrTargetFailed)
	}

	if err := r.ensureImportPVC(ctx, pvcTemplate); err != nil {
		logger.Error(err, "failed to ensure target PVC")
		return ctrl.Result{}, err
	}
	pvc, err := GetScratchPVC(ctx, r.Client, r.dataImport.Namespace, pvcTemplate.Name)
	if err != nil {
		logger.Error(err, "failed to get target PVC")
		return ctrl.Result{}, err
	}

	return r.handlePVCImportStatus(ctx, pvc)
}

// handlePVCImportStatus drives the target PVC of a standalone import (Mode B) to completion. It mirrors
// handleTargetStatus but (1) honors spec.WaitForFirstConsumer — the PVC may be bound by the user's own
// workload, so a load pod is only forced when the user opted in — and (2) completes the import the moment
// the upload finishes, with no capture and no durable artifact.
func (r *DataImportReconciler) handlePVCImportStatus(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Checking target PVC status")

	status, err := CheckPVCStatus(ctx, r.Client, pvc, r.dataImport.Spec.WaitForFirstConsumer)
	if err != nil {
		logger.Error(err, "failed to check target PVC status")
		return ctrl.Result{}, err
	}

	// volumeMode is informational here; a not-yet-defaulted PVC simply leaves it unset for now.
	if volumeMode, err := GetPVCVolumeMode(pvc); err == nil {
		r.dataImport.Status.VolumeMode = volumeMode
	}

	switch status {
	case TargetStatusReady:
		if cond := common.GetCondition(r.dataImport.Status.Conditions, common.ConditionUploadFinished); cond == nil || cond.Status != metav1.ConditionTrue {
			logger.Info("Target PVC is bound, awaiting data upload")
			meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
				Type:               string(common.ConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             string(common.ReasonPending),
				Message:            "Target PVC bound, awaiting data upload",
				ObservedGeneration: r.dataImport.Generation,
			})
			return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
		}
		// Mode B is done once the bytes land in the user PVC: no capture, no durable artifact.
		logger.Info("Upload finished, standalone PVC import completed")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionCompleted),
			Status:             metav1.ConditionTrue,
			Reason:             string(common.ReasonCompleted),
			Message:            fmt.Sprintf("Data import completed into PVC %s", pvc.Name),
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{}, nil
	case TargetStatusPending:
		logger.Info("Target PVC is pending")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPVCCreated),
			Message:            "Target PVC is created",
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	case TargetStatusNeedConsumer:
		logger.Info("Target PVC needs consumer, ensuring dummy Job")
		if err := r.ensureDummyJob(ctx, pvc); err != nil {
			logger.Error(err, "Failed to create dummy Job")
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPVCCreated),
			Message:            "Target PVC is created, dummy Job created to bind PVC",
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	case TargetStatusFailed:
		return ctrl.Result{}, fmt.Errorf("target PVC is in Failed state: %w", ErrTargetFailed)
	}

	return ctrl.Result{}, nil
}

// ensureDataArtifact captures the bound scratch PVC into a durable cluster-scoped VolumeSnapshotContent
// via a VolumeCaptureRequest, pins the artifact's reclaim policy to Retain, anchors it to the import's
// lifetime via the import ObjectKeeper, and records it in status.dataArtifactRef + Completed. The capture
// mode is auto-detected from the spec StorageClass (snapshot-capable -> Snapshot); a non-snapshot-capable
// StorageClass fails closed. It requeues while the VolumeCaptureRequest is in progress. DataImport never
// becomes the artifact's controller owner (storage-foundation's VCR retainer is); see object_keeper.go /
// artifact.go.
func (r *DataImportReconciler) ensureDataArtifact(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	mode, expectedKind, err := r.resolveSnapshotCaptureMode(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("%w: %w", ErrTargetFailed, err)
	}

	// Ensure the import keeper exists (with a populated UID) BEFORE the artifact is produced, so the
	// non-controller ownerRef can be attached as soon as the artifact appears — no window where the
	// artifact is anchored only to the VCR.
	keeper, res, err := r.ensureObjectKeeper(ctx)
	if err != nil || keeper == nil {
		return res, err
	}

	vcr, err := r.ensureVolumeCaptureRequest(ctx, pvc, mode)
	if err != nil {
		return ctrl.Result{}, err
	}
	if failed, reason := volumeCaptureRequestFailed(vcr); failed {
		return ctrl.Result{}, fmt.Errorf("%w: VolumeCaptureRequest failed: %s", ErrTargetFailed, reason)
	}
	if !volumeCaptureRequestReady(vcr) {
		logger.Info("VolumeCaptureRequest not ready yet, waiting", "name", vcr.GetName())
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "Capturing imported volume into data artifact",
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	}

	artifact, err := volumeCaptureArtifact(vcr, string(pvc.UID), expectedKind)
	if err != nil {
		// A produced VCR with a malformed/mismatched dataRef is a contract violation, not transient.
		return ctrl.Result{}, fmt.Errorf("%w: %w", ErrTargetFailed, err)
	}
	if err := r.pinArtifactRetain(ctx, artifact); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureArtifactKeptByKeeper(ctx, artifact, keeper); err != nil {
		return ctrl.Result{}, err
	}

	r.dataImport.Status.DataArtifactRef = artifact
	meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
		Type:               string(common.ConditionCompleted),
		Status:             metav1.ConditionTrue,
		Reason:             string(common.ReasonCompleted),
		Message:            fmt.Sprintf("Data import completed: produced %s %s", artifact.Kind, artifact.Name),
		ObservedGeneration: r.dataImport.Generation,
	})
	return ctrl.Result{}, nil
}

func (r *DataImportReconciler) cleanupDataImport(ctx context.Context) error {
	logger := log.FromContext(ctx)
	logger.Info("Cleaning up DataImport")

	// TODO: should we keep PVC if population is not finished successfully?
	// NOTE: if we delete DataImport before population is started, PVC is kept in
	// PENDING

	// Delete dummy Job if it exists
	jobName := types.NamespacedName{
		Namespace: r.dataImport.Namespace,
		Name:      r.names.DummyJobName,
	}
	isJobExists, err := common.DeleteJob(ctx, r.Client, jobName)
	if err != nil {
		logger.Error(err, "Failed to delete dummy Job")
		return err
	}
	if isJobExists {
		logger.Info("Dummy Job exists")
	}

	// Check if server deployment stopped
	deploymentName := types.NamespacedName{
		Namespace: r.Config.ControllerNamespace,
		Name:      r.names.DeployName,
	}

	deploy := &appsv1.Deployment{}
	err = r.Client.Get(ctx, deploymentName, deploy)
	isServerDeploymentExists := false
	if err != nil && !kubeerrors.IsNotFound(err) {
		logger.Error(err, "Failed to get server Deployment")
		return err
	} else if err == nil {
		logger.Info("Server Deployment exists")
		isServerDeploymentExists = true
	}

	// Delete publish resources if created
	isPublishExists, err := r.deletePublish(ctx)
	if err != nil {
		return err
	}
	if isPublishExists {
		logger.Info("Ingress or service exists")
	}

	// Check if we ready to shutdown or try again
	if isJobExists || isServerDeploymentExists || isPublishExists {
		logger.Info("Not all resources are deleted")
		return nil
	}

	// Remove the import finalizer from the PVC. RemovePVCFinalizer never deletes the PVC, so the volume
	// is preserved in both modes — which is exactly the contract for Mode B (the user's PVC is the
	// product). The PVC name differs by mode: Mode A's scratch PVC is named after the DataImport, while
	// Mode B's PVC is named by the user's pvcTemplate.
	pvcName := r.dataImport.Name
	if r.isStandalonePVCImport() {
		if tmpl := r.dataImport.Spec.TargetRef.PvcTemplate; tmpl != nil && tmpl.Name != "" {
			pvcName = tmpl.Name
		}
	}
	logger.Info("Removing PVC finalizer")
	err = RemovePVCFinalizer(
		ctx,
		r.Client,
		types.NamespacedName{Namespace: r.dataImport.Namespace, Name: pvcName},
		dev1alpha1.StorageManagerFinalizerName,
	)
	if err != nil {
		logger.Error(err, "Failed to remove finalizer from TargetRef")
		return err
	}

	logger.Info("Removing DataImport finalizer")
	common.RemoveFinalizer(ctx, r.Client, r.dataImport, dev1alpha1.StorageManagerFinalizerName)
	return nil
}

func (r *DataImportReconciler) ensureDummyJob(ctx context.Context, target client.Object) error {
	logger := log.FromContext(ctx)
	logger.Info("Creating dummy Job to bind PVC")

	// Generate job name based on target name
	jobName := r.names.DummyJobName
	resourceName := types.NamespacedName{
		Namespace: r.dataImport.Namespace,
		Name:      r.dataImport.Name,
	}

	container, err := MakeDummyContainer(ctx, r.Client, DummyContainerCfg{
		ConfigMapName: types.NamespacedName{
			Namespace: r.Config.ControllerNamespace,
			Name:      common.CongigMapName,
		},
		PvcName: types.NamespacedName{
			Namespace: target.GetNamespace(),
			Name:      target.GetName(),
		},
		ResourceName: resourceName,
		VolumeName:   "pvc",
	})

	if err != nil {
		logger.Error(err, "Failed to make dummy container")
		return err
	}

	volumes := common.MakeVolumes("pvc", target.GetName(), false)

	podSpec := corev1.PodSpec{
		ServiceAccountName: "default",
		ImagePullSecrets:   []corev1.LocalObjectReference{{Name: common.ImagePullSecretsName}},
		Containers:         []corev1.Container{*container},
		Volumes:            volumes,
		RestartPolicy:      corev1.RestartPolicyNever,
	}

	jobCfg := common.JobCfg{
		PodSpec: podSpec,
		JobName: types.NamespacedName{
			// NOTE: create Job in user's namespace (same as DataImport resource), because pod
			// can only reference PVC in the same namespace.
			Namespace: target.GetNamespace(),
			Name:      jobName,
		},
		ResourceName:          resourceName,
		LabelApplicationValue: dev1alpha1.LabelDataImportValue,
	}

	err = common.EnsureJob(ctx, r.Client, jobCfg)
	return err
}

func (r *DataImportReconciler) ensurePublish(ctx context.Context) error {
	publicURL, err := publish.EnsurePublicURL(
		ctx,
		r.Client,
		r.Reader,
		publish.HeadlessServiceCfg{
			ServiceName:           types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.HeadlessServiceName},
			DeploymentName:        r.names.DeployName,
			LabelApplicationValue: dev1alpha1.LabelDataImportValue,
		},
		publish.IngressCfg{
			IngressName:      types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.IngressResourceName},
			ServiceName:      types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.HeadlessServiceName},
			OriginIngress:    types.NamespacedName{Namespace: r.Config.OriginIngressNamespace, Name: common.OriginIngressName},
			TargetSecretName: common.IngressSecretName,
			Path:             fmt.Sprintf("/%s/%s/%s", r.dataImport.Namespace, r.names.TargetKindShort, r.names.TargetName),
			CorsAllowMethods: "PUT, POST, HEAD, OPTIONS",
		})
	if err != nil {
		return err
	}

	r.dataImport.Status.PublicURL = publicURL

	return nil
}

func (r *DataImportReconciler) deletePublish(ctx context.Context) (bool, error) {
	deleted, err := publish.DeletePublicResources(
		ctx,
		r.Client,
		types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.HeadlessServiceName},
		types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.IngressResourceName},
	)
	if err != nil {
		return deleted, err
	}

	r.dataImport.Status.PublicURL = ""

	return deleted, err
}

// updateDataImport persists all accumulated in-memory mutations (spec, metadata, status) in a single
// deferred call at the end of Reconcile. Splits the write into two API calls: one for the main object
// (spec/finalizers/labels/annotations) and one for the status subresource, because Kubernetes treats
// them as independent resources. Uses diStatusCopy to preserve status across the main Update call,
// since the API server response overwrites the in-memory status with the server-side value.
func updateDataImport(ctx context.Context, c client.Client, dataImportOld, dataImportNew *dev1alpha1.DataImport) error {
	if dataImportOld == nil || dataImportNew == nil {
		return nil
	}

	needStatusUpdate := false
	needUpdate := needDataImportUpdate(dataImportOld, dataImportNew)
	var diStatusCopy *dev1alpha1.DataExportImportStatus

	// Save status before the main Update: the API server response will overwrite in-memory status
	if !reflect.DeepEqual(dataImportOld.Status, dataImportNew.Status) {
		needStatusUpdate = true
		diStatusCopy = dataImportNew.Status.DeepCopy()
	}

	// Persist spec/metadata (non-status fields); updates ResourceVersion in dataImportNew
	if needUpdate {
		if err := c.Update(ctx, dataImportNew); err != nil {
			return fmt.Errorf("update DataImport failed: %w", err)
		}
	}

	// Restore the saved status (overwritten by Update response) and persist via status subresource
	if needStatusUpdate {
		dataImportNew.Status = *diStatusCopy
		if err := c.Status().Update(ctx, dataImportNew); err != nil {
			return fmt.Errorf("update DataImport status failed: %w", err)
		}
	}

	return nil
}

func needDataImportUpdate(dataImportOld, dataImportNew *dev1alpha1.DataImport) bool {
	return !reflect.DeepEqual(dataImportOld.Spec, dataImportNew.Spec) ||
		!reflect.DeepEqual(dataImportOld.Finalizers, dataImportNew.Finalizers) ||
		!reflect.DeepEqual(dataImportOld.Labels, dataImportNew.Labels) ||
		!reflect.DeepEqual(dataImportOld.Annotations, dataImportNew.Annotations)
}

// updateReadiness flips the Ready condition to True once the upload server (and the ingress, when the
// import is published) is up. It is upgrade-only: when the server is not ready yet it leaves the Ready
// condition untouched so the more specific progress reason set by ensureTarget (e.g. PVCCreated, or a
// target precondition) stays visible instead of being masked by a generic "awaiting server readiness".
// Ready=True is the signal the client waits on before streaming bytes into the scratch PVC, and in the
// populator flow that PVC is still Pending at that point, so this must run even while ensureTarget
// requeues.
//
// NOTE: this only handles the "not ready" -> "ready" transition; it does not downgrade Ready if the
// server later fails or is deleted.
func (r *DataImportReconciler) updateReadiness(ctx context.Context) error {
	// Once the upload has finished, the capture/complete phase owns the Ready condition: ensureTarget
	// reports "Capturing ..." while the VolumeCaptureRequest runs and then Completed. The upload server
	// may still be briefly up during capture, so promoting Ready=True off it here would mask that
	// progress and momentarily signal readiness for an import that is not yet usable. Stop managing
	// readiness past UploadFinished.
	if cond := common.GetCondition(r.dataImport.Status.Conditions, common.ConditionUploadFinished); cond != nil && cond.Status == metav1.ConditionTrue {
		return nil
	}

	isServerReady, err := common.IsDeploymentReady(
		ctx,
		r.Client,
		types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.DeployName})
	if err != nil {
		return err
	}

	reason := common.ReasonPodReady
	message := "Server is ready"
	isReady := isServerReady

	if r.dataImport.Spec.Publish {
		isIngressReady, err := publish.IsPublishReady(
			ctx,
			r.Client,
			types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.IngressResourceName})
		if err != nil {
			return err
		}

		if isIngressReady {
			reason = common.ReasonIngressReady
			message = "Ingress is ready"
		}

		isReady = isServerReady && isIngressReady
	}

	if !isReady {
		return nil
	}

	meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
		Type:               string(common.ConditionReady),
		Status:             metav1.ConditionTrue,
		Reason:             string(reason),
		Message:            message,
		ObservedGeneration: r.dataImport.Generation,
	})

	return nil
}
