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
	"k8s.io/client-go/util/retry"
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
	//   2) finalizeDataImportStatus — projects phase + completionTimestamp from the final condition set
	//   3) updateDataImport — diffs against the original snapshot and persists only real changes
	dataImportOriginal := dataImport.DeepCopy()
	defer func() {
		mutateReadyByErr(dataImport, err)
		r.finalizeDataImportStatus(dataImport, err)
		updateErr := updateDataImport(ctx, r.Client, dataImportOriginal, dataImport)
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

	// Server-side resource names (upload deployment, service, ingress, dummy job) are always keyed on
	// the DataImport identity with the PVC short kind, independent of the import mode and of the actual
	// PVC name (PopulateData names the scratch PVC after the DataImport; CreatePVC names it from
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

		if cleanupErr := r.cleanupDataImport(ctx); cleanupErr != nil {
			return ctrl.Result{}, fmt.Errorf("%w: %w", ErrCleanupFailed, cleanupErr)
		}
		return ctrl.Result{}, nil
	}

	// Idle-TTL expiry: the importer pod reported serverState=IdleExpired (idle >= spec.ttl with no
	// in-flight upload — the pod enforces the window using --ttl=spec.ttl), OR a legacy object was already
	// expired under the old condition-based mechanism (Ready=False/Expired). This is the terminal Expired
	// outcome. Tear down the server-side infrastructure but keep the CR and its finalizer so the garbage
	// collector can delete it after the retention window (the deletion branch then performs the final
	// finalizer cleanup). Setting Ready=Expired also makes the volume populator tear down the upload
	// Deployment it owns. Checking this BEFORE the active pipeline prevents a migrated legacy-expired
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
		if _, cleanupErr := r.teardownImportInfra(ctx); cleanupErr != nil {
			return ctrl.Result{}, fmt.Errorf("%w: %w", ErrCleanupFailed, cleanupErr)
		}
		return ctrl.Result{}, nil
	}

	// One-shot terminal (Completed or Failed): the import is never resumed (VMOP model). Keep the body
	// inert so a later event cannot restart the pipeline under a terminal phase — which would contradict
	// the status and reset the GC retention clock (completionTimestamp is stamped once, at the real
	// terminal). Completed leaves its artifact and Failed produced none; the CR (and its finalizer) is
	// kept for the GC, whose Delete drives the deletion branch above. Expired is handled by its own branch
	// (it still needs idempotent teardown/retry), so it is excluded here.
	if p := common.Phase(r.dataImport.Status.Phase); p.IsTerminal() && p != common.PhaseExpired {
		logger.Info("DataImport is terminal, skipping reconcile", "phase", p)
		return ctrl.Result{}, nil
	}

	common.EnsureFinalizer(ctx, r.Client, r.dataImport, dev1alpha1.StorageManagerFinalizerName)

	// The pod no longer writes conditions; translate its Finished signal into the controller-owned
	// UploadFinished condition. The populator (cmd/main.go) and the capture step both gate on
	// UploadFinished=True, so this must run before ensureTarget inspects it.
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

// ensureSnapshotImportTarget runs the PopulateData pipeline: derive scratch-PVC parameters from
// spec.storageParams -> ensure the scratch PVC (the volume the imported bytes land in) -> once it
// is bound, produce the durable data artifact (VolumeSnapshotContent). It returns a RequeueAfter while
// waiting on out-of-band preconditions (PVC bind, VolumeCaptureRequest completion) that the controller
// does not watch.
func (r *DataImportReconciler) ensureSnapshotImportTarget(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Terminal: once the artifact is produced the import is done. Re-affirm Completed (idempotent) and
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

	// Scratch-PVC parameters come straight from the spec (provided by d8, mirrored from the source
	// xxxSnapshot.status); the leaf's captured manifest is no longer downloaded on import. Invalid spec
	// parameters are a terminal, user-fixable error.
	params, err := scratchVolumeParamsFromSpec(r.dataImport.Spec)
	if err != nil {
		logger.Error(err, "invalid scratch volume parameters in spec")
		return ctrl.Result{}, fmt.Errorf("%w: %w: %w", ErrTerminal, ErrTargetFailed, err)
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

// ensureScratchPVC creates/updates the internal scratch PVC sized and shaped from the spec parameters.
// The PVC is populated by the upload path (populator + published endpoint); the produced data artifact
// is captured from it once bound.
func (r *DataImportReconciler) ensureScratchPVC(ctx context.Context, params scratchVolumeParams) error {
	return r.ensureImportPVC(ctx, scratchPVCTemplate(r.dataImport.Name, params))
}

// ensureImportPVC creates/updates the PVC the imported bytes land in from the given template, wiring its
// DataSourceRef to this DataImport so the volume populator picks it up and brings up the upload endpoint.
// Both modes share it: PopulateData passes an internally-derived scratch template (named after the
// DataImport), CreatePVC passes the user's spec.pvcTemplate (named by the user).
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
		PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: name},
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

// ensurePVCImportTarget runs the CreatePVC pipeline: ensure the user-described target PVC
// (spec.pvcTemplate) -> once it is bound and the upload has finished, mark the import Completed. Unlike
// PopulateData it derives nothing from a scratch template, does not gate on snapshot capability, and
// produces no durable artifact (status.data stays empty); the PVC itself is the product and is preserved
// on cleanup.
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

// handlePVCImportStatus drives the target PVC of a standalone import (CreatePVC) to completion. It mirrors
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
		// CreatePVC is done once the bytes land in the user PVC: no capture, no durable artifact.
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
// lifetime via the import ObjectKeeper, and records it in status.data.artifactRef + Completed. The capture
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

	r.dataImport.Status.Data = &dev1alpha1.DataExportImportData{ArtifactRef: artifact}
	meta.SetStatusCondition(&r.dataImport.Status.Conditions, metav1.Condition{
		Type:               string(common.ConditionCompleted),
		Status:             metav1.ConditionTrue,
		Reason:             string(common.ReasonCompleted),
		Message:            fmt.Sprintf("Data import completed: produced %s %s", artifact.Kind, artifact.Name),
		ObservedGeneration: r.dataImport.Generation,
	})
	return ctrl.Result{}, nil
}

// teardownImportInfra deletes the controller-owned server-side resources (dummy Job, publish
// Service/Ingress) and reports whether ALL server-side infrastructure is gone — including the upload
// Deployment, which the volume populator owns and tears down when it observes Ready=Expired/Deleted. It
// never touches finalizers, so it is safe to call both on idle expiry (keep the CR for the GC) and as the
// first phase of deletion cleanup. It is idempotent: not-found resources count as already gone.
func (r *DataImportReconciler) teardownImportInfra(ctx context.Context) (allGone bool, err error) {
	logger := log.FromContext(ctx)

	// Delete dummy Job if it exists
	jobName := types.NamespacedName{
		Namespace: r.dataImport.Namespace,
		Name:      r.names.DummyJobName,
	}
	isJobExists, err := common.DeleteJob(ctx, r.Client, jobName)
	if err != nil {
		logger.Error(err, "Failed to delete dummy Job")
		return false, err
	}
	if isJobExists {
		logger.Info("Dummy Job exists")
	}

	// Check if server deployment stopped (the populator owns and deletes it)
	deploymentName := types.NamespacedName{
		Namespace: r.Config.ControllerNamespace,
		Name:      r.names.DeployName,
	}

	deploy := &appsv1.Deployment{}
	getErr := r.Client.Get(ctx, deploymentName, deploy)
	isServerDeploymentExists := false
	if getErr != nil && !kubeerrors.IsNotFound(getErr) {
		logger.Error(getErr, "Failed to get server Deployment")
		return false, getErr
	} else if getErr == nil {
		logger.Info("Server Deployment exists")
		isServerDeploymentExists = true
	}

	// Delete publish resources if created
	isPublishExists, err := r.deletePublish(ctx)
	if err != nil {
		return false, err
	}
	if isPublishExists {
		logger.Info("Ingress or service exists")
	}

	if isJobExists || isServerDeploymentExists || isPublishExists {
		logger.Info("Not all resources are deleted")
		return false, nil
	}
	return true, nil
}

func (r *DataImportReconciler) cleanupDataImport(ctx context.Context) error {
	logger := log.FromContext(ctx)
	logger.Info("Cleaning up DataImport")

	// TODO: should we keep PVC if population is not finished successfully?
	// NOTE: if we delete DataImport before population is started, PVC is kept in
	// PENDING

	allGone, err := r.teardownImportInfra(ctx)
	if err != nil {
		return err
	}
	// Check if we ready to shutdown or try again
	if !allGone {
		return nil
	}

	// Remove the import finalizer from the PVC. RemovePVCFinalizer never deletes the PVC, so the volume
	// is preserved in both modes — which is exactly the contract for CreatePVC (the user's PVC is the
	// product). The PVC name differs by mode: PopulateData's scratch PVC is named after the DataImport,
	// while CreatePVC's PVC is named by the user's pvcTemplate.
	pvcName := r.dataImport.Name
	if r.isCreatePVCMode() {
		if tmpl := r.dataImport.Spec.PvcTemplate; tmpl != nil && tmpl.Name != "" {
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
		logger.Error(err, "Failed to remove finalizer from target PVC")
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

// updateDataImport persists the accumulated in-memory mutations (spec/metadata, status) at the end of
// Reconcile. Metadata (spec/finalizers/labels/annotations) and the status subresource are independent
// resources, so each gets its own API call. Both are wrapped in RetryOnConflict with a fresh GET: the
// importer pod writes the pod-owned status fields (serverState, accessTimestamp, url, ca) concurrently
// (heartbeat every 30s), so a blind Status().Update off the reconcile-start snapshot would clobber a
// concurrent serverState=Finished/IdleExpired write and hang the import. Instead the controller-owned
// status is re-applied onto the freshly read object while the pod-owned fields are carried over from the
// latest server-side state.
func updateDataImport(ctx context.Context, c client.Client, dataImportOld, dataImportNew *dev1alpha1.DataImport) error {
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
			merged.Url = fresh.Status.Url
			merged.CA = fresh.Status.CA
			merged.AccessTimestamp = fresh.Status.AccessTimestamp
			merged.ServerState = fresh.Status.ServerState
			fresh.Status = *merged
			if err := c.Status().Update(ctx, fresh); err != nil {
				return fmt.Errorf("update DataImport status failed: %w", err)
			}
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
// is the signal the client waits on before streaming bytes into the scratch PVC, and in the populator
// flow that PVC is still Pending at that point, so this must run even while ensureTarget requeues.
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
	// published). That is the authoritative endpoint-readiness signal; the pod is brought up out of band
	// by the volume populator, so the controller keys readiness off the pod's own report.
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
