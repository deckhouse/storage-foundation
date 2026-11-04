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

	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/sds-common-lib/conditions"
	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
	"github.com/deckhouse/storage-foundation/common/publish"
	"github.com/deckhouse/storage-foundation/images/data-manager-controller/internal/controllers/readyevents"
)

// Sentinel errors are used as typed markers so callers can distinguish failure
// categories with errors.Is without parsing message strings.
// mutateReadyByErr maps each sentinel to the corresponding Ready condition reason.
var (
	ErrCleanupFailed = errors.New("cleanup failed")
	ErrTargetFailed  = errors.New("target failed")
	// ErrTerminal marks a reconcile error as un-retryable (bad spec, a failed capture, a malformed
	// artifact). It is composed with a specific sentinel (e.g. ErrTargetFailed) so mutateReadyByErr still
	// derives the Ready reason. The deferred block turns it into a terminal phase=Failed and stops
	// requeueing; transient errors (API/get failures) are NOT wrapped and keep the object retryable
	// (a permanently-pending import is legal, never garbage-collected).
	ErrTerminal = errors.New("terminal")
)

type DataImportReconciler struct {
	Client client.Client
	Reader client.Reader
	Config *config.Options
	// Dynamic talks to CRDs SVDM has no Go types for (VolumeCaptureRequest, ObjectKeeper) without
	// compiling in their modules.
	Dynamic dynamic.Interface
	// Recorder records an Event on the DataImport when its Ready condition moves. Nil records nothing.
	Recorder conditions.EventRecorder
	// Now returns the current time; it is injectable so tests can assert completionTimestamp
	// deterministically. Defaults to metav1.Now.
	Now        func() metav1.Time
	dataImport *dev1alpha1.DataImport
	names      common.Names
}

// now returns the reconciler clock, defaulting to metav1.Now when unset.
func (r *DataImportReconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}
	return metav1.Now()
}

func (r *DataImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	logger.Info("Start reconciling DataImport resource")

	dataImport := &dev1alpha1.DataImport{}
	err = r.Client.Get(ctx, req.NamespacedName, dataImport)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			logger.Info("DataImport resource not found, it may have been deleted; releasing what it left behind")
			// A DataImport deleted inside its first reconcile is gone before its own finalizer was ever
			// written, so its deletion branch never ran. See releaseOrphanedResources.
			if err := r.releaseOrphanedResources(ctx, req.NamespacedName); err != nil {
				return ctrl.Result{}, err
			}
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
	//   2) finalizeDataImportStatus — projects phase + completionTimestamp from the final condition set
	//   3) updateDataImport — diffs against the original snapshot and persists only real changes
	dataImportOriginal := dataImport.DeepCopy()
	defer func() {
		mutateReadyByErr(dataImport, err)
		r.finalizeDataImportStatus(dataImport, err)
		updateErr := updateDataImport(ctx, r.Client, r.Recorder, dataImportOriginal, dataImport)
		switch {
		case errors.Is(err, ErrTerminal):
			// Un-retryable failure, now recorded as phase=Failed. Do not requeue the error; surface only a
			// (retryable) status-write failure so the terminal status still gets persisted on retry.
			result = ctrl.Result{}
			err = updateErr
		case err != nil:
			// Retryable reconcile error: its backoff governs the requeue, so drop any RequeueAfter left in
			// result and surface the update failure alongside the original error.
			result = ctrl.Result{}
			err = errors.Join(err, updateErr)
		case updateErr != nil && kubeerrors.IsConflict(updateErr):
			// updateDataImport already retried on conflict; a surviving conflict is benign (a concurrent
			// writer won the race). Requeue soon instead of escalating to an error+backoff.
			result = ctrl.Result{RequeueAfter: dataImportRequeueInterval}
		case updateErr != nil:
			err = updateErr
		}
	}()

	// TODO: do we need to validate DataImport?

	// Server-side resource names (upload deployment, service, ingress) are always keyed on the DataImport
	// identity with the PVC short kind, independent of the import mode and of the actual PVC name
	// (PopulateData names the scratch PVC after the DataImport; CreatePVC names its target PVC from
	// pvcTemplate).
	r.names = common.NewNames(dev1alpha1.KindPVC, dataImport.Name, dataImport.Namespace, dataImport.Name)
	r.dataImport = dataImport

	// Migrate legacy objects onto the current condition catalog before any status write: pre-existing
	// DataImports carry a stale "Expired" condition (seeded by the previous controller) that the
	// narrowed CRD condition-type enum no longer permits; leaving it in the atomic conditions list would
	// make every Status().Update fail enum validation.
	common.StripConditionsNotIn(&r.dataImport.Status,
		common.ConditionReady, common.ConditionUploadFinished, common.ConditionCompleted)

	// Migrate a legacy Ready=True reason (PodReady/IngressReady) onto the current catalog (ServerReady):
	// a mid-flight object never has its Ready reason rewritten otherwise, and the narrowed reason enum
	// would reject the first status write. Rewriting the reason in place keeps lastTransitionTime.
	if ready := meta.FindStatusCondition(r.dataImport.Status.Conditions, string(common.ConditionReady)); ready != nil &&
		ready.Status == metav1.ConditionTrue && ready.Reason != string(common.ReasonServerReady) {
		ready.Reason = string(common.ReasonServerReady)
	}

	// Ensure the base conditions of the DataImport catalog exist (independently, so a migrated legacy
	// object that already has Ready still gets Completed). UploadFinished is created on demand from
	// serverState=Finished.
	if meta.FindStatusCondition(r.dataImport.Status.Conditions, string(common.ConditionReady)) == nil {
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "Started",
			ObservedGeneration: r.dataImport.Generation,
		})
	}
	if meta.FindStatusCondition(r.dataImport.Status.Conditions, string(common.ConditionCompleted)) == nil {
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionCompleted),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonInProgress),
			Message:            "Data import in progress",
			ObservedGeneration: r.dataImport.Generation,
		})
	}

	if r.dataImport.DeletionTimestamp != nil {
		logger.Info("DataImport resource is marked for deletion")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonDeleted),
			Message:            "DataImport is going to be deleted",
			ObservedGeneration: r.dataImport.Generation,
		})

		done, cleanupErr := r.cleanupDataImport(ctx)
		if cleanupErr != nil {
			return ctrl.Result{}, fmt.Errorf("%w: %w", ErrCleanupFailed, cleanupErr)
		}
		if !done {
			// Nothing watches Pods here, so a live importer pod produces no event to resume on — self-requeue.
			return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
		}
		return ctrl.Result{}, nil
	}

	// Idle-TTL expiry: the importer pod reported serverState=IdleExpired (idle >= spec.ttl with no
	// in-flight upload — the pod enforces the window using --ttl=spec.ttl), OR a legacy object was already
	// expired under the old condition-based mechanism (Ready=False/Expired). This is the terminal Expired
	// outcome. Tear down the server-side infrastructure but keep the CR and its finalizer so the garbage
	// collector can delete it after the retention window (the deletion branch then performs the final
	// finalizer cleanup). Checking this BEFORE the active pipeline prevents a migrated legacy-expired
	// object from being resurrected into a new import.
	if isDataImportExpired(r.dataImport) {
		logger.Info("DataImport idle TTL expired")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonExpired),
			Message:            "DataImport idle timeout expired",
			ObservedGeneration: r.dataImport.Generation,
		})
		allGone, cleanupErr := r.teardownImportInfra(ctx)
		if cleanupErr != nil {
			return ctrl.Result{}, fmt.Errorf("%w: %w", ErrCleanupFailed, cleanupErr)
		}
		if !allGone {
			// Same as the deletion branch: a live importer pod produces no event to resume on.
			return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
		}
		return ctrl.Result{}, nil
	}

	// Failed-phase housekeeping, CreatePVC only. A CreatePVC import fails terminally when it observes the
	// user deleting its target PVC, so our finalizer would otherwise hold that delete until the garbage
	// collector removes the DataImport. It runs only once Failed is persisted, on purpose: released any
	// earlier, a later pass could not tell a PVC the user deleted from one that was never created, and
	// would recreate it.
	if common.Phase(r.dataImport.Status.Phase) == common.PhaseFailed && r.isCreatePVCMode() {
		if err := r.reconcileTargetPVCRelease(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}

	// One-shot terminal (Completed or Failed): the import is never resumed (VMOP model). Keep the body
	// inert so a later event cannot restart the pipeline under a terminal phase — which would contradict
	// the status and reset the GC retention clock (completionTimestamp is stamped once, at the real
	// terminal). Completed leaves its artifact and Failed produced none; the CR (and its finalizer) is
	// kept for the GC, whose Delete drives the deletion branch above. Expired is handled by its own branch
	// (it still needs idempotent teardown/retry), so it is excluded here. The Failed-phase target-PVC
	// release above is the sole exception that still runs here, and it cannot restart the pipeline.
	if p := common.Phase(r.dataImport.Status.Phase); p.IsTerminal() && p != common.PhaseExpired {
		logger.Info("DataImport is terminal, skipping reconcile", "phase", p)
		return ctrl.Result{}, nil
	}

	common.EnsureFinalizer(ctx, r.Client, r.dataImport, dev1alpha1.StorageManagerFinalizerName)

	// The pod no longer writes conditions; translate its Finished signal into the controller-owned
	// UploadFinished condition. The capture step (PopulateData) and the handover step (CreatePVC) both
	// gate on UploadFinished=True, so this must run before ensureTarget inspects it.
	if common.ServerState(r.dataImport.Status.ServerState) == common.ServerStateFinished {
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionUploadFinished),
			Status:             metav1.ConditionTrue,
			Reason:             string(common.ReasonUploadFinished),
			Message:            "Client upload finished; producing artifact",
			ObservedGeneration: r.dataImport.Generation,
		})
	}

	result, err = r.ensureTarget(ctx)
	if err != nil {
		return result, err
	}

	if r.dataImport.Spec.Publish {
		err = r.ensurePublish(ctx)
	} else {
		_, err = r.deletePublish(ctx)
	}
	if err != nil {
		return result, err
	}

	// updateReadiness must run even while ensureTarget requeues: in CreatePVC the scratch PVC stays Pending
	// for the whole upload window and the target PVC only binds at handover, so ensureTarget keeps
	// requeueing with a progress reason throughout the upload.
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

// isDataImportExpired reports whether the import has terminally idle-expired: the importer pod reported
// serverState=IdleExpired, or a legacy object was expired under the old condition-based mechanism
// (Ready=False/Expired). Recognizing the legacy form keeps a migrated pre-upgrade expired object terminal
// instead of resurrecting it into a fresh import.
func isDataImportExpired(di *dev1alpha1.DataImport) bool {
	if common.ServerState(di.Status.ServerState) == common.ServerStateIdleExpired {
		return true
	}
	ready := meta.FindStatusCondition(di.Status.Conditions, string(common.ConditionReady))
	return ready != nil && ready.Status == metav1.ConditionFalse && ready.Reason == string(common.ReasonExpired)
}

// computeDataImportPhase derives the coarse-grained phase. A terminal phase is STICKY (once Completed /
// Expired / Failed, it never reverts even if a later reconcile hits a transient state), which keeps
// completionTimestamp meaningful and prevents the GC age from being reset. Terminal transitions are
// EXPLICIT: Failed only from an ErrTerminal reconcile error (never from an ambiguous condition reason
// that is also used for retryable failures), Expired from the idle signal, Completed from the produced
// artifact. Precedence: deletion > sticky-terminal > completed > expired > failed > ready > pending.
func (r *DataImportReconciler) computeDataImportPhase(di *dev1alpha1.DataImport, reconcileErr error) common.Phase {
	if di.DeletionTimestamp != nil {
		return common.PhaseTerminating
	}
	if common.Phase(di.Status.Phase).IsTerminal() {
		return common.Phase(di.Status.Phase)
	}
	if meta.IsStatusConditionTrue(di.Status.Conditions, string(common.ConditionCompleted)) {
		return common.PhaseCompleted
	}
	if isDataImportExpired(di) {
		return common.PhaseExpired
	}
	if errors.Is(reconcileErr, ErrTerminal) {
		return common.PhaseFailed
	}
	// Actively progressing (endpoint served, or the upload finished and the artifact is being produced) —
	// keep Ready rather than dipping to Pending during the capture window.
	if meta.IsStatusConditionTrue(di.Status.Conditions, string(common.ConditionReady)) ||
		meta.IsStatusConditionTrue(di.Status.Conditions, string(common.ConditionUploadFinished)) {
		return common.PhaseReady
	}
	return common.PhasePending
}

// finalizeDataImportStatus computes phase, keeps the terminal conditions consistent with it (idempotently,
// so a settled terminal object produces no status churn), and stamps completionTimestamp once. It runs in
// the deferred block after the reconcile body and mutateReadyByErr, so it always sees the definitive
// state. The controller is the sole writer of phase, completionTimestamp and all conditions.
func (r *DataImportReconciler) finalizeDataImportStatus(di *dev1alpha1.DataImport, reconcileErr error) {
	if di == nil {
		return
	}
	phase := r.computeDataImportPhase(di, reconcileErr)

	switch phase {
	case common.PhaseCompleted:
		// The endpoint is done; move Ready off ServerReady to the terminal Completed reason.
		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonCompleted),
			Message:            "Data import completed",
			ObservedGeneration: di.Generation,
		})
	case common.PhaseExpired:
		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonExpired),
			Message:            "DataImport idle timeout expired",
			ObservedGeneration: di.Generation,
		})
		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionCompleted),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonExpired),
			Message:            "DataImport idle timeout expired before completion",
			ObservedGeneration: di.Generation,
		})
	case common.PhaseFailed:
		message := "DataImport failed"
		if ready := meta.FindStatusCondition(di.Status.Conditions, string(common.ConditionReady)); ready != nil {
			message = ready.Message
		}
		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionCompleted),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonFailed),
			Message:            message,
			ObservedGeneration: di.Generation,
		})
	}

	di.Status.Phase = string(phase)
	common.SetCompletionTimestampOnce(&di.Status, phase, r.now())
}

// isCreatePVCMode reports whether this DataImport populates a preserved volume (CreatePVC): the imported
// bytes are written into a created PVC (pvcTemplate), with no durable artifact produced. The mode is the
// explicit spec.mode discriminator (defaulting to CreatePVC); the alternative is PopulateData.
func (r *DataImportReconciler) isCreatePVCMode() bool {
	return r.dataImport.Spec.EffectiveMode() == dev1alpha1.DataImportModeCreatePVC
}

// ensureTarget dispatches to the import pipeline for the active spec.mode: CreatePVC imports into a
// preserved, newly created PVC; PopulateData captures the imported bytes into a durable
// VolumeSnapshotContent.
func (r *DataImportReconciler) ensureTarget(ctx context.Context) (ctrl.Result, error) {
	if r.isCreatePVCMode() {
		return r.ensurePVCImportTarget(ctx)
	}
	return r.ensureSnapshotImportTarget(ctx)
}

// ensureSnapshotImportTarget runs the PopulateData pipeline: bring up the internal scratch PVC and its
// importer Deployment (the upload phase), then stop the importer and capture the scratch volume into the
// durable data artifact once the client finishes uploading (the capture phase).
//
// The capture-phase check runs BEFORE the upload phase ensures the PVC: once UploadFinished is True the
// scratch PVC may already be gone (a prior pass captured it but failed to persist status before
// returning), and re-creating it here would blindly re-open the upload endpoint for data that was already
// captured.
func (r *DataImportReconciler) ensureSnapshotImportTarget(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// a. Terminal: once the artifact is produced the import is done. Re-affirm Completed (idempotent) and
	// stop, so completed DataImports don't re-derive the scratch PVC on every publish/server event until
	// TTL expiry.
	if r.dataImport.Status.Data != nil && r.dataImport.Status.Data.ArtifactRef != nil {
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:   string(common.ConditionCompleted),
			Status: metav1.ConditionTrue,
			Reason: string(common.ReasonCompleted),
			Message: fmt.Sprintf("Data import completed: produced %s %s",
				r.dataImport.Status.Data.ArtifactRef.Kind, r.dataImport.Status.Data.ArtifactRef.Name),
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{}, nil
	}

	// b. Scratch-PVC parameters come straight from the spec (provided by d8, mirrored from the source
	// xxxSnapshot.status); the leaf's captured manifest is no longer downloaded on import. Invalid spec
	// parameters are a terminal, user-fixable error.
	params, err := scratchVolumeParamsFromSpec(r.dataImport.Spec)
	if err != nil {
		logger.Error(err, "invalid scratch volume parameters in spec")
		return ctrl.Result{}, fmt.Errorf("%w: %w: %w", ErrTerminal, ErrTargetFailed, err)
	}

	// c. Fail fast: core import supports snapshot-capable CSI only. Snapshot capability is a static
	// property of spec.storageClassName, knowable before any bytes are uploaded, so reject a
	// non-snapshot-capable StorageClass up front instead of after a full (potentially multi-GiB) upload.
	// The check is re-run at capture time (ensureDataArtifact) to guard against the annotation changing
	// mid-flight; transient API errors here are returned and recover on requeue.
	if _, _, err := r.resolveSnapshotCaptureMode(ctx); err != nil {
		logger.Error(err, "storage class is not snapshot-capable")
		return ctrl.Result{}, fmt.Errorf("%w: %w", ErrTargetFailed, err)
	}

	pvcKey := types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.ImportScratchPVCName}

	if meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionUploadFinished)) {
		return r.captureSnapshotImportTarget(ctx, pvcKey)
	}

	return r.uploadImportTarget(ctx, scratchPVCTemplate(r.names.ImportScratchPVCName, params), "")
}

// captureSnapshotImportTarget runs the PopulateData capture phase, reached once the client's upload has
// finished (UploadFinished=True).
//
// The importer must be fully stopped — Deployment deleted and every importer pod terminated
// (importerPodsGone) — before the scratch volume is captured, on every code path.
func (r *DataImportReconciler) captureSnapshotImportTarget(ctx context.Context, scratchPvcKey types.NamespacedName) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	stopped, err := r.stopImporter(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !stopped {
		logger.Info("Stopping the upload server before capturing the volume")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "Stopping the upload server before capturing the volume",
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	}

	scratchPvc, err := GetPVC(ctx, r.Client, scratchPvcKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	if scratchPvc != nil && scratchPvc.Status.Phase != corev1.ClaimBound {
		return ctrl.Result{}, fmt.Errorf("%w: %w: internal scratch PVC %s is %s, expected Bound after a finished upload",
			ErrTerminal, ErrTargetFailed, scratchPvcKey, scratchPvc.Status.Phase)
	}

	if scratchPvc != nil {
		// Informational only: keep the previously reported value rather than failing the capture.
		if volumeMode, err := GetPVCVolumeMode(scratchPvc); err == nil {
			r.dataImport.Status.VolumeMode = volumeMode
		}
	}

	return r.ensureDataArtifact(ctx, scratchPvc)
}

// uploadImportTarget runs the upload phase shared by both import modes: bring up the internal scratch
// PVC and its importer Deployment, so the client can stream bytes into it. With no nodeHint, the importer
// pod being the PVC's own first consumer is what makes a WaitForFirstConsumer StorageClass bind the volume.
func (r *DataImportReconciler) uploadImportTarget(ctx context.Context, template *dev1alpha1.PersistentVolumeClaimTemplateSpec, nodeHint string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pvc, err := r.ensureScratchPVC(ctx, template, nodeHint)
	if err != nil {
		return ctrl.Result{}, err
	}

	volumeMode, err := GetPVCVolumeMode(pvc)
	if err != nil {
		logger.Error(err, "failed to get scratch PVC volume mode")
		return ctrl.Result{}, err
	}
	r.dataImport.Status.VolumeMode = volumeMode

	if err := r.ensureImporterDeployment(ctx, pvc); err != nil {
		// common.MakeServerContainer/EnsureDeployment already log the underlying failure; avoid a
		// second log line for the same error.
		return ctrl.Result{}, err
	}

	switch internalPVCStatus(pvc) {
	case TargetStatusReady:
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "Internal volume bound, awaiting data upload",
			ObservedGeneration: r.dataImport.Generation,
		})
	case TargetStatusPending:
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPVCCreated),
			Message:            "Internal volume created, awaiting the upload server pod to bind it",
			ObservedGeneration: r.dataImport.Generation,
		})
	case TargetStatusFailed:
		return ctrl.Result{}, fmt.Errorf("internal scratch PVC %s/%s is in phase %s: %w", pvc.Namespace, pvc.Name, pvc.Status.Phase, ErrTargetFailed)
	}
	return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
}

// scratchVolumeParams holds the parameters used to size and shape the scratch PVC the imported bytes are
// written into. They are sourced from DataImportSpec (no longer from the leaf's captured manifest).
type scratchVolumeParams struct {
	StorageClassName string
	VolumeMode       string
	Size             resource.Quantity
}

// scratchVolumeParamsFromSpec validates and converts the PopulateData scratch volume parameters from
// spec.storageParams. The params, their storageClass and their size are required (the scratch PVC
// cannot be built without them); volumeMode is optional and defaults to Filesystem downstream.
func scratchVolumeParamsFromSpec(spec dev1alpha1.DataImportSpec) (scratchVolumeParams, error) {
	tmpl := spec.StorageParams
	if tmpl == nil {
		return scratchVolumeParams{}, fmt.Errorf("spec.storageParams is required for PopulateData")
	}
	if tmpl.StorageClassName == "" {
		return scratchVolumeParams{}, fmt.Errorf("spec.storageParams.storageClassName is required")
	}
	if tmpl.Size == "" {
		return scratchVolumeParams{}, fmt.Errorf("spec.storageParams.size is required")
	}
	size, err := resource.ParseQuantity(tmpl.Size)
	if err != nil {
		return scratchVolumeParams{}, fmt.Errorf("parse spec.storageParams.size %q: %w", tmpl.Size, err)
	}
	return scratchVolumeParams{
		StorageClassName: tmpl.StorageClassName,
		VolumeMode:       tmpl.VolumeMode,
		Size:             size,
	}, nil
}

// ensureScratchPVC creates/updates the internal volume the imported bytes land in:
//   - it lives in the controller namespace, so the VolumeCaptureRequest that captures it can share that
//     namespace, and nothing operational appears in the user's namespace;
//   - it has no DataSourceRef, so it is dynamically provisioned and the importer Deployment is its first
//     consumer.
//
// nodeHint, when non-empty, is stamped as the scheduler's node-selection annotation so the scratch PVC
// lands on the node a WaitForFirstConsumer target PVC's own consumer already selected. It is applied only
// on create — EnsurePVC's existing-PVC path never touches metadata, so a hint that changes later is never
// re-patched onto an already-scheduled or bound PVC.
func (r *DataImportReconciler) ensureScratchPVC(ctx context.Context, template *dev1alpha1.PersistentVolumeClaimTemplateSpec, nodeHint string) (*corev1.PersistentVolumeClaim, error) {
	if nodeHint != "" {
		annotations := make(map[string]string, len(template.Annotations)+1)
		for k, v := range template.Annotations {
			annotations[k] = v
		}
		annotations[annSelectedNode] = nodeHint

		hinted := *template
		hinted.Annotations = annotations
		template = &hinted
	}

	return EnsurePVC(ctx, r.Client, r.Config.ControllerNamespace,
		types.NamespacedName{Namespace: r.dataImport.Namespace, Name: r.dataImport.Name},
		template, nil)
}

// ensureImportPVC creates the CreatePVC target PVC from spec.pvcTemplate. DataSourceRef names a kind the
// StorageClass's provisioner does not recognize, so the claim is left parked Pending instead of being
// dynamically provisioned — the reconciler fills the volume itself and hands it over later
// (reconcileHandover). An existing PVC of that name is never routed through EnsurePVC's
// handleExistingPVC, which would either overwrite a stranger's PVC or attempt an immutable-spec update
// that no-ops until the PVC binds and then fails permanently with "spec is immutable after creation".
func (r *DataImportReconciler) ensureImportPVC(ctx context.Context, pvcTemplate *dev1alpha1.PersistentVolumeClaimTemplateSpec) (*corev1.PersistentVolumeClaim, error) {
	existing, err := getTargetPVC(ctx, r.Client, r.dataImport)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !isTargetPVCOwnedBy(existing, r.dataImport) {
			return nil, fmt.Errorf("%w: %w: target PVC %s/%s already exists and is not owned by this DataImport",
				ErrTerminal, ErrTargetFailed, existing.Namespace, existing.Name)
		}
		// The finalizer that keeps this PVC observable is released only once Failed is persisted
		// (reconcileTargetPVCRelease), on a later pass — never here, and never as a write on this pass.
		if existing.DeletionTimestamp != nil {
			return nil, fmt.Errorf("%w: %w: target PVC %s/%s is being deleted",
				ErrTerminal, ErrTargetFailed, existing.Namespace, existing.Name)
		}
		return existing, nil
	}

	apiGroup := dev1alpha1.APIGroup
	dataSourceRef := &corev1.TypedObjectReference{
		APIGroup: &apiGroup,
		Kind:     dataImportKind,
		Name:     r.dataImport.Name,
	}
	resourceName := types.NamespacedName{Namespace: r.dataImport.Namespace, Name: r.dataImport.Name}
	return EnsurePVC(ctx, r.Client, r.dataImport.Namespace, resourceName, pvcTemplate, dataSourceRef)
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
		PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: name},
		PersistentVolumeClaimSpec: dev1alpha1.PersistentVolumeClaimSpec{
			AccessModes:      []dev1alpha1.PersistentVolumeAccessMode{dev1alpha1.ReadWriteOnce},
			Resources:        dev1alpha1.VolumeResourceRequirements{Requests: dev1alpha1.ResourceList{dev1alpha1.ResourceStorage: params.Size}},
			StorageClassName: &sc,
			VolumeMode:       &volumeMode,
		},
	}
}

// ensurePVCImportTarget runs the CreatePVC pipeline. It produces no durable artifact (status.data stays
// empty); the target PVC itself is the product and is preserved on cleanup.
func (r *DataImportReconciler) ensurePVCImportTarget(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Terminal: once Completed, stop re-ensuring the PVC on every publish/server event until TTL expiry.
	if meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)) {
		return ctrl.Result{}, nil
	}

	pvcTemplate := r.dataImport.Spec.PvcTemplate
	if pvcTemplate == nil || pvcTemplate.Name == "" {
		// CRD CEL enforces this; guard anyway since the controller would otherwise build an unnamed PVC.
		return ctrl.Result{}, fmt.Errorf("%w: %w: pvcTemplate with metadata.name is required for CreatePVC", ErrTerminal, ErrTargetFailed)
	}

	targetPVC, err := r.ensureImportPVC(ctx, pvcTemplate)
	if err != nil {
		logger.Error(err, "failed to ensure target PVC")
		return ctrl.Result{}, err
	}

	return r.handlePVCImportStatus(ctx, targetPVC)
}

// handlePVCImportStatus drives the CreatePVC target PVC through the shared upload phase and the handover
// to completion. Every decision is re-derived from live objects, never from a persisted status flag, so a
// settled state issues no writes and a resumed one picks up where it left off.
func (r *DataImportReconciler) handlePVCImportStatus(ctx context.Context, targetPVC *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// volumeMode is informational here; a not-yet-defaulted PVC simply leaves it unset for now.
	if volumeMode, err := GetPVCVolumeMode(targetPVC); err == nil {
		r.dataImport.Status.VolumeMode = volumeMode
	}

	scratchPVC, err := GetPVC(ctx, r.Client, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.ImportScratchPVCName})
	if err != nil {
		return ctrl.Result{}, err
	}
	uploadFinished := meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionUploadFinished))

	if scratchPVC == nil {
		return r.handleCreatePVCWithoutScratchPVC(ctx, targetPVC, uploadFinished)
	}

	if !uploadFinished {
		return r.reconcileCreatePVCUpload(ctx, targetPVC)
	}

	stopped, err := r.stopImporter(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !stopped {
		logger.Info("Stopping the upload server before handing the volume over to the target PVC")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "Stopping the upload server before handing the volume over to the target PVC",
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	}

	result, err := reconcileHandover(ctx, r.Client, r.dataImport, targetPVC, scratchPVC)
	if err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	// reconcileHandover reached ctrl.Result{}, nil only by deleting the scratch PVC after confirming the
	// target PVC is Bound to the volume it staged — the one state left to report.
	logger.Info("Upload finished, standalone PVC import completed")
	applyCreatePVCCompleted(r.dataImport, targetPVC)
	return ctrl.Result{}, nil
}

// handleCreatePVCWithoutScratchPVC decides the CreatePVC outcome when no internal scratch PVC is
// currently staging an upload: either nothing has started yet, or a handover finished — on the first pass
// or a resumed one after an interruption — leaving target PVC Bound, no scratch PVC.
func (r *DataImportReconciler) handleCreatePVCWithoutScratchPVC(ctx context.Context, targetPVC *corev1.PersistentVolumeClaim, uploadFinished bool) (ctrl.Result, error) {
	if targetPVC.Status.Phase == corev1.ClaimBound {
		if !uploadFinished {
			return ctrl.Result{}, fmt.Errorf(
				"%w: %w: target PVC %s/%s is already Bound, but not by this import",
				ErrTerminal, ErrTargetFailed, targetPVC.Namespace, targetPVC.Name)
		}
		applyCreatePVCCompleted(r.dataImport, targetPVC)
		return ctrl.Result{}, nil
	}

	if uploadFinished {
		// The scratch PVC staged and finished an upload that never reached the target: never restart the
		// upload silently onto a fresh scratch PVC, which would recreate bytes nobody asked for again.
		return ctrl.Result{}, fmt.Errorf(
			"%w: %w: the data previously uploaded for target PVC %s/%s is gone; recreate the import",
			ErrTerminal, ErrTargetFailed, targetPVC.Namespace, targetPVC.Name)
	}

	return r.reconcileCreatePVCUpload(ctx, targetPVC)
}

// applyCreatePVCCompleted marks the import Completed=True: the target PVC now holds the imported bytes
// and nothing is staging them anymore.
func applyCreatePVCCompleted(di *dev1alpha1.DataImport, targetPVC *corev1.PersistentVolumeClaim) {
	meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
		Type:               string(common.ConditionCompleted),
		Status:             metav1.ConditionTrue,
		Reason:             string(common.ReasonCompleted),
		Message:            fmt.Sprintf("Data import completed into PVC %s", targetPVC.Name),
		ObservedGeneration: di.Generation,
	})
}

// reconcileCreatePVCUpload brings up (or continues) the CreatePVC upload phase. Only a `nil` StorageClass
// name waits before creating the scratch PVC, because the cluster's default class may still be assigned
// retroactively; an empty string or a real name both proceed to the class lookup and surface its failure
// as a retryable error.
func (r *DataImportReconciler) reconcileCreatePVCUpload(ctx context.Context, targetPVC *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if targetPVC.Spec.StorageClassName == nil {
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:   string(common.ConditionReady),
			Status: metav1.ConditionFalse,
			Reason: string(common.ReasonPVCCreated),
			Message: fmt.Sprintf("Target PVC %s/%s has no StorageClass yet; waiting for the cluster's default StorageClass to be assigned",
				targetPVC.Namespace, targetPVC.Name),
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	}

	classIsWFC, err := isWaitForFirstConsumer(ctx, r.Client, targetPVC)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("%w: %w", ErrTargetFailed, err)
	}

	nodeHint, wait := computeScratchNodeHint(targetPVC, classIsWFC, r.dataImport.Spec.WaitForFirstConsumer)
	if wait {
		logger.Info("Target PVC is awaiting a consumer to select a node")
		meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
			Type:   string(common.ConditionReady),
			Status: metav1.ConditionFalse,
			Reason: string(common.ReasonPVCCreated),
			Message: fmt.Sprintf("Target PVC %s/%s is awaiting a consumer to select a node",
				targetPVC.Namespace, targetPVC.Name),
			ObservedGeneration: r.dataImport.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	}

	return r.uploadImportTarget(ctx, computeCreatePVCScratchTemplate(r.names.ImportScratchPVCName, targetPVC), nodeHint)
}

// ensureDataArtifact captures the bound scratch PVC into a durable cluster-scoped VolumeSnapshotContent
// via a VolumeCaptureRequest, pins the artifact's reclaim policy to Retain, anchors it to the import's
// lifetime via the import ObjectKeeper, and records it in status.data.artifactRef (alongside the scratch
// volume's observed status.data.fsType, the one field that outlives the volume) + Completed. The capture
// mode is auto-detected from the spec StorageClass (snapshot-capable -> Snapshot); a non-snapshot-capable
// StorageClass fails closed. It requeues while the VolumeCaptureRequest is in progress. DataImport never
// becomes the artifact's controller owner (storage-foundation's VCR retainer is); see object_keeper.go /
// artifact.go. pvc may be nil: the capture phase may be re-entered after the scratch PVC was already
// deleted by a prior pass that failed before its status write landed; a nil pvc is only valid once the
// VCR already exists (ensureVolumeCaptureRequest enforces this).
func (r *DataImportReconciler) ensureDataArtifact(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	mode, expectedKind, err := r.resolveSnapshotCaptureMode(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("%w: %w", ErrTargetFailed, err)
	}

	// Ensure the import keeper exists (with a populated UID) BEFORE the artifact is produced, so the
	// non-controller ownerRef can be attached as soon as the artifact appears — no window where the
	// artifact is anchored only to the VCR. It also anchors the VCR's own ownerRef now (buildVolumeCaptureRequest),
	// so the keeper UID is doubly load-bearing.
	keeper, res, err := r.ensureObjectKeeper(ctx)
	if err != nil || keeper == nil {
		return res, err
	}

	vcr, err := r.ensureVolumeCaptureRequest(ctx, pvc, keeper, mode)
	if err != nil {
		return ctrl.Result{}, err
	}
	if failed, reason := volumeCaptureRequestFailed(vcr); failed {
		return ctrl.Result{}, fmt.Errorf("%w: %w: VolumeCaptureRequest failed: %s", ErrTerminal, ErrTargetFailed, reason)
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

	artifact, err := volumeCaptureArtifact(vcr, expectedKind)
	if err != nil {
		// A produced VCR with a malformed/mismatched dataRef is a contract violation, not transient.
		return ctrl.Result{}, fmt.Errorf("%w: %w: %w", ErrTerminal, ErrTargetFailed, err)
	}
	if err := r.pinArtifactRetain(ctx, artifact); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureArtifactKeptByKeeper(ctx, artifact, keeper); err != nil {
		return ctrl.Result{}, err
	}

	// The scratch PVC is no longer needed now that the artifact is safely captured. Completed is set
	// only once it's actually deleted — Completed is sticky-terminal, so a swallowed failure here
	// would never get another chance and would leak the PVC forever. A nil pvc means it is already gone
	// (a prior pass deleted it before failing to persist status) — nothing left to delete, and with the
	// claim already gone its filesystem type is unobservable, which status.data.fsType reports as unset.
	var fsType string
	if pvc != nil {
		// Observe the scratch volume's filesystem type BEFORE the delete below destroys it. This is the last
		// point where the value exists: the PV goes away with the claim, the captured artifact records no
		// filesystem type, and nothing downstream ever sees this volume again. The observation is best-effort by
		// construction (scratchVolumeFSType cannot fail), so it can neither block nor delay publishing the
		// artifact reference — which is already durable at this point.
		fsType = scratchVolumeFSType(ctx, r.Client, pvc)

		if err := DeleteScratchPVC(ctx, r.Client, pvc); err != nil {
			return ctrl.Result{}, fmt.Errorf("delete scratch PVC: %w", err)
		}
	}

	r.dataImport.Status.Data = &dev1alpha1.DataExportImportData{ArtifactRef: artifact, FsType: fsType}
	meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
		Type:               string(common.ConditionCompleted),
		Status:             metav1.ConditionTrue,
		Reason:             string(common.ReasonCompleted),
		Message:            fmt.Sprintf("Data import completed: produced %s %s", artifact.Kind, artifact.Name),
		ObservedGeneration: r.dataImport.Generation,
	})
	return ctrl.Result{}, nil
}

// teardownImportInfra tears down every server-side resource this DataImport owns, for either mode: the
// importer Deployment (and its pods), the publish resources, the CA secret, and the internal scratch PVC.
// The scratch PVC is deleted unconditionally at any point in the pipeline: before a CreatePVC handover it
// is the only claimant of the imported volume, so deleting it reclaims the volume under the class's own
// policy — the bytes were never handed over; after handover the volume's claimRef already names the
// target PVC, so deleting the scratch PVC no longer reclaims it.
func (r *DataImportReconciler) teardownImportInfra(ctx context.Context) (allGone bool, err error) {
	importerGone, err := r.stopImporter(ctx)
	if err != nil {
		return false, err
	}

	isPublishExists, err := r.deletePublish(ctx)
	if err != nil {
		return false, err
	}

	secretExists, err := deleteCASecret(ctx, r.Client, types.NamespacedName{
		Namespace: r.Config.ControllerNamespace,
		Name:      r.names.CASecretName,
	})
	if err != nil {
		return false, fmt.Errorf("delete CA secret: %w", err)
	}

	// Never delete a PVC while an importer pod may still hold the mount — the same consistency invariant
	// that gates the capture phase (see captureSnapshotImportTarget).
	if !importerGone {
		return false, nil
	}

	pvc, err := GetPVC(ctx, r.Client, types.NamespacedName{Name: r.names.ImportScratchPVCName, Namespace: r.Config.ControllerNamespace})
	if err != nil {
		return false, fmt.Errorf("get internal scratch PVC: %w", err)
	}
	if pvc != nil {
		if err := DeleteScratchPVC(ctx, r.Client, pvc); err != nil {
			return false, fmt.Errorf("delete internal scratch PVC: %w", err)
		}
	}

	return !isPublishExists && !secretExists, nil
}

// deleteCASecret deletes the importer's CA secret and reports whether it existed. Mirrors
// common.DeleteDeployment's shape. NotFound is success.
func deleteCASecret(ctx context.Context, c client.Client, key types.NamespacedName) (bool, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	if err := c.Delete(ctx, secret); err != nil {
		if kubeerrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// deleteVolumeCaptureRequest removes this import's VCR explicitly rather than relying on the ownerRef
// cascade through the import ObjectKeeper: the keeper is only collected once the ObjectKeeper controller
// observes the DataImport is gone, and a VCR that never completed carries no completionTimestamp, so the
// VCR garbage collector's fail-safe skips it forever.
func (r *DataImportReconciler) deleteVolumeCaptureRequest(ctx context.Context) error {
	name := volumeCaptureRequestName(r.dataImport.UID)
	err := r.Dynamic.Resource(volumeCaptureRequestGVR).Namespace(r.Config.ControllerNamespace).
		Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !kubeerrors.IsNotFound(err) {
		return fmt.Errorf("delete VolumeCaptureRequest %s: %w", name, err)
	}
	return nil
}

// cleanupDataImport runs the deletion-time teardown: tear down every server-side resource, explicitly
// delete the VCR, then release the DataImport finalizer. It never deletes the CreatePVC target PVC (the
// user's product) — only its import finalizer is removed. Returns done=false when teardown is not finished
// yet (e.g. a live importer pod), so the caller can requeue instead of dropping the object.
func (r *DataImportReconciler) cleanupDataImport(ctx context.Context) (done bool, err error) {
	logger := log.FromContext(ctx)
	logger.Info("Cleaning up DataImport")

	// TODO: should we keep PVC if population is not finished successfully?
	// NOTE: if we delete DataImport before population is started, PVC is kept in
	// PENDING

	allGone, err := r.teardownImportInfra(ctx)
	if err != nil {
		return false, err
	}
	// Check if we ready to shutdown or try again
	if !allGone {
		return false, nil
	}

	if err := r.deleteVolumeCaptureRequest(ctx); err != nil {
		return false, err
	}

	// CreatePVC only: drop the import finalizer from the user's target PVC, never the PVC itself — it is
	// the user's product. PopulateData's internal scratch PVC is already gone (teardownImportInfra).
	if r.isCreatePVCMode() {
		if tmpl := r.dataImport.Spec.PvcTemplate; tmpl != nil && tmpl.Name != "" {
			logger.Info("Removing PVC finalizer")
			if err := RemovePVCFinalizer(
				ctx,
				r.Client,
				r.dataImport,
				dev1alpha1.StorageManagerFinalizerName,
			); err != nil {
				logger.Error(err, "Failed to remove finalizer from target PVC")
				return false, err
			}
		}
	}

	logger.Info("Removing DataImport finalizer")
	common.RemoveFinalizer(ctx, r.Client, r.dataImport, dev1alpha1.StorageManagerFinalizerName)
	return true, nil
}

// importProtocolAllowHeaders lists the request headers of the upload protocol itself, on top of
// publish.CORSStandardAllowHeaders. The first five are what the exporter demands of a PUT: a request
// without them is refused with 400 before the handler runs (CheckRequiredHeaders in
// images/data-exporter/internal/middleware/http.go). X-Attribute-ModTime and X-LinkTarget are optional
// per file but carry filesystem metadata the importer cannot reconstruct (see
// images/data-exporter/internal/httpiohelpers/file.go).
//
// An omission in THIS list fails earlier and far more quietly than that 400: nothing is stripped from
// the request and no request is made. The preflight answers with an Access-Control-Allow-Headers that
// lacks the name, the browser rejects the whole thing as a CORS error, and the actual PUT never leaves
// it — so the exporter logs no request at all.
//
// Only a browser is affected, because only a browser enforces CORS. nginx neither strips a request
// header that is missing from the allow-list nor hides a response header that is missing from the
// expose-list; a CLI client sees both regardless. Note this does NOT mean "d8 does not use this
// ingress": `d8 data import upload --publish` uploads to exactly this status.publicURL with exactly
// these headers. It just never consults Access-Control-Allow-Headers, so a green CLI upload through
// this ingress proves nothing about the list being complete — that has to be checked from a browser.
//
// The two lists are joined by the controller, never hand-copied.
const importProtocolAllowHeaders = "X-Offset,X-Content-Length,X-Attribute-Permissions,X-Attribute-Uid,X-Attribute-Gid,X-Attribute-ModTime,X-LinkTarget"

// importCORSAllowHeaders / importCORSExposeHeaders are the ingress CORS header contract of the upload
// endpoint. The client that needs it is the console, which uploads to status.publicURL from origin
// console.<domain>; `d8 data import upload --publish` uses the same publicURL, but as a non-browser
// client it is bound by neither list.
//
// Every exposed header is load-bearing, and each is easy to lose without breaking a single request:
//   - X-Next-Offset is where the next chunk starts, written on success by import_block/handler.go and
//     import_filesystem/handler.go;
//   - X-Expected-Offset accompanies the 409 raised on an offset mismatch (import_block/handler.go,
//     import_filesystem/handler.go). The other 409 — a competing writer — carries no headers at all,
//     so without X-Expected-Offset the client cannot tell the two apart: a recoverable
//     reposition-and-continue becomes an unrecoverable failure;
//   - X-Device-Size is the size of the target block device, answered on the same HEAD as X-Next-Offset
//     (import_block/handler.go) and reported nowhere else in this API, so it is how a client learns how
//     much room it has. Writing past the end is refused with a bare 416 that carries no headers
//     whatsoever (import_block/handler.go, both the offset and the body-length branch), so a client that
//     cannot read the size can neither check "this archive is larger than the volume" up front nor
//     explain the failure afterwards — it only finds out at the end of a transfer that may have run for
//     hours.
const (
	importCORSAllowHeaders  = publish.CORSStandardAllowHeaders + "," + importProtocolAllowHeaders
	importCORSExposeHeaders = "X-Next-Offset,X-Expected-Offset,X-Device-Size"
)

// makeIngressCfg builds the Ingress configuration of the upload endpoint. It is a separate function so
// the published contract (CORS header lists, body size limit) is unit-testable without a cluster.
func (r *DataImportReconciler) makeIngressCfg() publish.IngressCfg {
	return publish.IngressCfg{
		IngressName:      types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.IngressResourceName},
		ServiceName:      types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.HeadlessServiceName},
		OriginIngress:    types.NamespacedName{Namespace: r.Config.OriginIngressNamespace, Name: common.OriginIngressName},
		TargetSecretName: common.IngressSecretName,
		Path:             fmt.Sprintf("/%s/%s/%s", r.dataImport.Namespace, r.names.TargetKindShort, r.names.TargetName),
		CorsAllowMethods: "PUT, POST, HEAD, OPTIONS",
		// The browser uploads cross-origin, so every protocol header needs an explicit CORS mandate:
		// allow-headers on the way in, expose-headers on the way back.
		CorsAllowHeaders:  importCORSAllowHeaders,
		CorsExposeHeaders: importCORSExposeHeaders,
		// Uploads are PUT with a body; the block/filesystem import protocol is resumable (offset-based,
		// X-Next-Offset), so clients chunk. Cap a single request at 64m instead of the controller default
		// (1m, which 413s any real upload) — bounded to avoid unbounded nginx request buffering on the
		// ingress node while still allowing arbitrarily large volumes via chunked PUTs.
		ProxyBodySize: "64m",
		// The importer signs its serving certificate for the headless Service with its own CA.
		BackendCASecretName: r.names.CASecretName,
		BackendServerName:   common.BackendServerName(r.names.HeadlessServiceName, r.Config.ControllerNamespace),
	}
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
		r.makeIngressCfg())
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

// updateDataImport persists the accumulated in-memory mutations (spec/metadata, status) at the end of
// Reconcile. Metadata (spec/finalizers/labels/annotations) and the status subresource are independent
// resources, so each gets its own API call. Both are wrapped in RetryOnConflict with a fresh GET: the
// importer pod writes the pod-owned status fields (serverState, accessTimestamp, url, ca) concurrently
// (heartbeat every 30s), so a blind Status().Update off the reconcile-start snapshot would clobber a
// concurrent serverState=Finished/IdleExpired write and hang the import. Instead the controller-owned
// status is re-applied onto the freshly read object while the pod-owned fields are carried over from the
// latest server-side state. A status write that moves Ready records an Event through rec, which may be
// nil, against the conditions it replaced, so a write that is retried or fails records none.
func updateDataImport(ctx context.Context, c client.Client, rec conditions.EventRecorder, dataImportOld, dataImportNew *dev1alpha1.DataImport) error {
	if dataImportOld == nil || dataImportNew == nil {
		return nil
	}

	// The controller only ever changes its own finalizer (and status); it never writes spec/labels/
	// annotations. So the metadata write is reduced to reconciling that single finalizer onto the fresh
	// object, which avoids clobbering concurrent third-party edits (spec, labels, foregroundDeletion).
	wantFinalizer := common.ContainsString(dataImportNew.Finalizers, dev1alpha1.StorageManagerFinalizerName)
	needMeta := wantFinalizer != common.ContainsString(dataImportOld.Finalizers, dev1alpha1.StorageManagerFinalizerName)
	needStatus := !reflect.DeepEqual(dataImportOld.Status, dataImportNew.Status)
	if !needMeta && !needStatus {
		return nil
	}

	statusCopy := dataImportNew.Status.DeepCopy()
	key := types.NamespacedName{Namespace: dataImportNew.Namespace, Name: dataImportNew.Name}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &dev1alpha1.DataImport{}
		if err := c.Get(ctx, key, fresh); err != nil {
			return err
		}

		if needMeta {
			fresh.Finalizers = common.ReconcileFinalizer(fresh.Finalizers, dev1alpha1.StorageManagerFinalizerName, wantFinalizer)
			if err := c.Update(ctx, fresh); err != nil {
				return fmt.Errorf("update DataImport failed: %w", err)
			}
		}

		if needStatus {
			merged := statusCopy.DeepCopy()
			// Preserve the pod-owned status fields from the latest server-side object; the controller
			// owns everything else (conditions, phase, completionTimestamp, publicURL, volumeMode, data).
			merged.URL = fresh.Status.URL
			merged.CA = fresh.Status.CA
			merged.AccessTimestamp = fresh.Status.AccessTimestamp
			merged.ServerState = fresh.Status.ServerState
			before := fresh.Status.Conditions
			fresh.Status = *merged
			if err := c.Status().Update(ctx, fresh); err != nil {
				return fmt.Errorf("update DataImport status failed: %w", err)
			}
			readyevents.Record(rec, fresh, before, fresh.Status.Conditions)
		}

		return nil
	})
	// The object was deleted out from under us (e.g. its finalizer was just removed and Kubernetes
	// finalized the deletion, or the GC deleted it): there is nothing left to persist — treat as success.
	return client.IgnoreNotFound(err)
}

// updateReadiness flips the Ready condition to True/ServerReady once the upload server reports it is
// serving (status.serverState=Ready, written by the importer pod) and, for a published import, the
// ingress is up too. It is upgrade-only: when the endpoint is not ready yet it leaves the Ready condition
// untouched so the more specific progress reason set by ensureTarget (e.g. PVCCreated, or a target
// precondition) stays visible instead of being masked by a generic "awaiting server readiness". Ready=True
// is the signal the client waits on before streaming bytes into the scratch/target PVC, and that PVC may
// still be Pending at that point in both modes — the controller brings up the same importer Deployment
// directly for both — so this must run even while ensureTarget requeues.
//
// NOTE: this only handles the "not ready" -> "ready" transition; it does not downgrade Ready if the
// server later fails or is deleted (the terminal Expired/Failed transitions own the downgrade).
func (r *DataImportReconciler) updateReadiness(ctx context.Context) error {
	// Once the upload has finished, the capture/complete phase owns the Ready condition: ensureTarget
	// reports "Capturing ..." while the VolumeCaptureRequest runs and then Completed. The upload server
	// may still be briefly up during capture, so promoting Ready=True off it here would mask that
	// progress and momentarily signal readiness for an import that is not yet usable. Stop managing
	// readiness past UploadFinished.
	if cond := common.GetCondition(r.dataImport.Status.Conditions, common.ConditionUploadFinished); cond != nil && cond.Status == metav1.ConditionTrue {
		return nil
	}

	// The importer pod publishes serverState=Ready once it is actually serving (listening + CA
	// published). That is the authoritative endpoint-readiness signal; the pod's own Deployment lifecycle
	// (creation, scheduling, image pull) is out of band from this reconcile, so the controller keys
	// readiness off the pod's own report rather than trying to infer it from the Deployment's status.
	if common.ServerState(r.dataImport.Status.ServerState) != common.ServerStateReady {
		return nil
	}

	if r.dataImport.Spec.Publish {
		isIngressReady, err := publish.IsPublishReady(
			ctx,
			r.Client,
			types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: r.names.IngressResourceName})
		if err != nil {
			return err
		}
		if !isIngressReady {
			return nil
		}
	}

	meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
		Type:               string(common.ConditionReady),
		Status:             metav1.ConditionTrue,
		Reason:             string(common.ReasonServerReady),
		Message:            "Server is ready",
		ObservedGeneration: r.dataImport.Generation,
	})

	return nil
}
