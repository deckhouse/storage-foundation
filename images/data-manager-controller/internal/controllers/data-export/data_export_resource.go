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

package dataexport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	// dev1alpha1 "hello-world/api/v1alpha1"
	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	. "github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
	"github.com/deckhouse/storage-foundation/common/publish"
	virtv1alpha2 "github.com/deckhouse/virtualization/api/core/v1alpha2"
	"github.com/deckhouse/virtualization/api/core/v1alpha2/vdcondition"
)

type DataexportReconciler struct {
	Client client.Client
	Reader client.Reader
	Config *config.Options
	// Dynamic and RESTMapper drive the resource-agnostic snapshot export path (C6): the target leaf is
	// any registered snapshot CR addressed by GroupKind, resolved to its SnapshotContent.dataRef
	// without compiling in domain types.
	Dynamic    dynamic.Interface
	RESTMapper meta.RESTMapper
	// Now returns the current time; it is injectable so tests can assert completionTimestamp
	// deterministically. Defaults to metav1.Now.
	Now func() metav1.Time
}

// dataExportRequeueInterval is the soft requeue delay used when a benign write conflict survives the
// update retry, so the reconcile is retried promptly without escalating to an error backoff.
const dataExportRequeueInterval = 10 * time.Second

// now returns the reconciler clock, defaulting to metav1.Now when unset.
func (r *DataexportReconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}
	return metav1.Now()
}

// pvRecoveryInfo holds validated PV annotation data needed for orphan cleanup and idempotency checks.
type pvRecoveryInfo struct {
	UserPVCNamespace      string
	UserPVCName           string
	UserPVCUID            string
	DataExportNamespace   string
	DataExportName        string
	DataExportUID         string
	TargetKindShort       string
	HashSuffix            string
	OriginalReclaimPolicy corev1.PersistentVolumeReclaimPolicy
}

// pvOwnerExpectation is what a caller can prove about the takeover the PV records. Namespace/name are
// always known; the UIDs are not — the orphan sweep starts from a parent that no longer exists, and a
// legacy takeover predates the UID annotations entirely. An empty expectation therefore means "no
// opinion" and skips the corresponding check rather than failing.
type pvOwnerExpectation struct {
	DataExportNamespace string
	DataExportName      string
	DataExportUID       types.UID
	SourcePVCUID        types.UID
}

const (
	DataExportInProgressKey        = "storage-foundation.deckhouse.io/data-export-in-progress"
	DataExportRequestAnnotationKey = "storage-foundation.deckhouse.io/data-export-request"

	SeverityWarning = "warning"
	SeverityError   = "error"
)

// Sentinel errors are used as typed markers so callers can distinguish failure
// categories with errors.Is without parsing message strings.
// mutateReadyByErr maps each sentinel to the corresponding Ready condition reason,
// making controller behavior explicit and testable.
var (
	ErrTargetNotFound   = errors.New("target not found")
	ErrTargetNotReady   = errors.New("target not ready")
	ErrPVConflict       = errors.New("pv conflict")
	ErrDeploymentFailed = errors.New("deployment failed")
	ErrCleanupFailed    = errors.New("cleanup failed")
	// ErrTerminal marks a reconcile error as un-retryable (an invalid spec / target). The deferred block
	// turns it into a terminal phase=Failed and stops requeueing. Transient errors (API/get failures) are
	// NOT wrapped and keep the object retryable (a permanently-pending export is legal, never GC'd).
	ErrTerminal = errors.New("terminal")

	// errTakeoverNotHealthy ends a reconcile whose verdict is already on the object: the state was
	// classified and recorded, and the pass has nothing left to do. It never leaves Reconcile — it only
	// buys a requeue, because a blocked export waits on something the controller does not watch (a
	// stranger's claim being removed, a volume reappearing), and without one it would sit at its barrier
	// until the resync.
	errTakeoverNotHealthy = errors.New("takeover state is not healthy")
)

func (r *DataexportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	log.Printf("Start reconciling DataExport resource: %s/%s\n", req.Namespace, req.Name)

	dataExport := &dev1alpha1.DataExport{}
	err = r.Client.Get(ctx, req.NamespacedName, dataExport)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			// DataExport resource not found, it may have been deleted after the event was received.
			log.Printf("DataExport resource %s/%s not found, checking for orphaned resources\n", req.Namespace, req.Name)
			blocked, err := r.removeOrphanResources(ctx, req.Namespace, req.Name)
			if blocked != nil {
				// There is no object left to report on, so the barrier lives in the log and in the fact
				// that the volume keeps its export marks until the way is clear.
				log.Printf("Orphan cleanup for %s/%s held by %s", req.Namespace, req.Name, blocked)
				return ctrl.Result{RequeueAfter: dataExportRequeueInterval}, nil
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, fmt.Errorf("failed to get DataExport resource from cache: %w", err)
	}

	// Copy the original state before any mutations.
	// The reconcile body mutates dataExport in-memory throughout the entire cycle
	// (conditions, finalizers, status fields, etc.) without persisting intermediate states.
	// The deferred function collects all mutations at the end:
	//   1) mutateReadyByErr - translates reconcile errors into Ready condition reasons
	//   2) updateDataExport - diffs against the original snapshot and persists only real changes
	dataExportOrig := dataExport.DeepCopy()
	defer func() {
		mutateReadyByErr(dataExport, err)
		r.finalizeDataExportStatus(dataExport, err)
		updateErr := r.updateDataExport(ctx, dataExportOrig, dataExport)
		switch {
		case errors.Is(err, ErrTerminal):
			// Un-retryable failure, now recorded as phase=Failed. Do not requeue the error; surface only a
			// (retryable) status-write failure so the terminal status still gets persisted on retry.
			result = ctrl.Result{}
			err = updateErr
		case err != nil:
			// Retryable reconcile error: its backoff governs the requeue, so drop any stale RequeueAfter
			// and surface the update failure alongside the original error.
			result = ctrl.Result{}
			err = errors.Join(err, updateErr)
		case updateErr != nil && kubeerrors.IsConflict(updateErr):
			// updateDataExport already retried on conflict; a surviving conflict is benign — requeue soon
			// instead of escalating to an error backoff.
			result = ctrl.Result{RequeueAfter: dataExportRequeueInterval}
		case updateErr != nil:
			err = updateErr
		}
	}()

	// Migrate legacy objects onto the current single-condition (Ready) catalog before any status write:
	// pre-existing DataExports carry a stale "Expired" condition the narrowed CRD condition-type enum no
	// longer permits; leaving it in the atomic conditions list would fail enum validation on every write.
	StripConditionsNotIn(&dataExport.Status, ConditionReady)

	// Migrate a legacy Ready=True reason (PodReady) onto the current catalog (ServerReady): a
	// fully-provisioned DataExport reaches Case 5, which never rewrites the Ready reason, so without this a
	// migrated object would keep PodReady forever and the narrowed reason enum would reject every write.
	if ready := meta.FindStatusCondition(dataExport.Status.Conditions, string(ConditionReady)); ready != nil &&
		ready.Status == metav1.ConditionTrue && ready.Reason != string(ReasonServerReady) {
		ready.Reason = string(ReasonServerReady)
	}

	// Case 1: Resource marked for delete. Deletion takes precedence over validation so a terminally-invalid
	// object (phase=Failed from a bad spec/target) stays deletable and the garbage collector can reap it.
	// Nothing is provisioned for an invalid spec, so if the target cannot even be classified there is
	// nothing to restore — just drop our finalizer to unblock deletion.
	if dataExport.DeletionTimestamp != nil {
		log.Printf("Case 1: DataExport resource %s/%s marked for delete", req.Namespace, req.Name)
		_, delTargetKindShort, delClassifyErr := classifyTargetRef(dataExport.Spec.TargetRef.Group, dataExport.Spec.TargetRef.Kind)
		if delClassifyErr != nil {
			log.Printf("DataExport %s/%s target unclassifiable and nothing provisioned; dropping finalizer to allow deletion", req.Namespace, req.Name)
			RemoveFinalizer(ctx, r.Client, dataExport, dev1alpha1.StorageManagerFinalizerName)
			return ctrl.Result{}, nil
		}
		delNames := NewNamesFromShort(delTargetKindShort, dataExport.Spec.TargetRef.Name, dataExport.Namespace, dataExport.Name)
		done, blocked, err := r.clearDataExportProviding(ctx, dataExport, delNames)
		switch {
		case err != nil:
			if errors.Is(err, ErrInvalidOriginalReclaimPolicy) {
				// PV was labeled as inconsistent (missing or corrupted original reclaimPolicy).
				// Stop reconcile without error or requeue — finalizer stays, admin must investigate.
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("%w: failed to restore configuration before DE: %w", ErrCleanupFailed, err)
		case blocked != nil:
			// Deletion is the only path allowed to drop the finalizer, and it may not do so while the
			// teardown is unfinished: that finalizer is what keeps the volume's way home reachable.
			log.Printf("DataExport %s/%s: deletion held by %s", req.Namespace, req.Name, blocked)
			setCleanupBlocked(dataExport, blocked)
			return ctrl.Result{RequeueAfter: dataExportRequeueInterval}, nil
		case !done:
			return ctrl.Result{RequeueAfter: dataExportRequeueInterval}, nil
		}
		RemoveFinalizer(ctx, r.Client, dataExport, dev1alpha1.StorageManagerFinalizerName)
		return ctrl.Result{}, nil
	}

	// Case 1b: a managed resource this export depends on was lost or replaced, and the object owes a
	// recovery before it may settle. This runs ahead of expiry, the terminal no-op and provisioning:
	// expiry teardown assumes the export claim still exists and would drop the finalizer that keeps the
	// recovery reachable, and provisioning would try to take the volume over a second time. It runs
	// before spec validation too — the discriminator is only ever set after provisioning succeeded, so a
	// spec that turned invalid afterwards must not strand a half-restored volume.
	if dataExport.Status.CleanupReason != "" {
		log.Printf("Case 1b: DataExport resource %s/%s owes managed-resource recovery (%s)",
			req.Namespace, req.Name, dataExport.Status.CleanupReason)
		return r.reconcileManagedResourceRecovery(ctx, dataExport)
	}

	err = r.validateDataExportSpec(ctx, dataExport)
	if err != nil {
		log.Printf("DataExport resource %s/%s spec validation failed: %v\n", req.Namespace, req.Name, err)
		if errors.Is(err, ErrTerminal) {
			// Genuine spec/target validation failure — terminal until the user corrects the spec. Record
			// Ready=ValidationFailed; the deferred maps ErrTerminal to phase=Failed and stops requeueing.
			meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
				Type:               string(ConditionReady),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: dataExport.Generation,
				Reason:             string(ReasonValidationFailed),
				Message:            err.Error(),
			})
			return ctrl.Result{}, err
		}
		// Transient failure (e.g. an API error while probing the VirtualDisk CRD) — retry without marking
		// the object Failed; leave the Ready reason untouched.
		return ctrl.Result{}, err
	}

	// Resolve the GroupKind targetRef to a stable short kind (pvc/vd/snap) used for deterministic
	// resource naming and orphan recovery. Classification failures (e.g. a bare VolumeSnapshotContent)
	// are permanent spec errors, surfaced like validation failures.
	_, targetKindShort, classifyErr := classifyTargetRef(dataExport.Spec.TargetRef.Group, dataExport.Spec.TargetRef.Kind)
	if classifyErr != nil {
		log.Printf("DataExport resource %s/%s targetRef invalid: %v\n", req.Namespace, req.Name, classifyErr)
		meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
			Type:               string(ConditionReady),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: dataExport.Generation,
			Reason:             string(ReasonValidationFailed),
			Message:            classifyErr.Error(),
		})
		// An invalid/forbidden targetRef is terminal until the spec is corrected: the deferred maps
		// ErrTerminal to phase=Failed and stops requeueing.
		return ctrl.Result{}, fmt.Errorf("%w: %w", ErrTerminal, classifyErr)
	}
	generatedNames := NewNamesFromShort(targetKindShort, dataExport.Spec.TargetRef.Name, dataExport.Namespace, dataExport.Name)

	// Case 2: DataExport idle-TTL expired — the exporter pod reported serverState=IdleExpired (idle >=
	// spec.ttl with no in-flight download; the pod enforces the window via --ttl=spec.ttl), OR a legacy
	// object was already expired under the old condition-based mechanism (Ready=False/Expired). Terminal
	// Expired outcome: restore the exported PV/PVC and tear down. Deletion of the CR itself is left to the
	// garbage collector after the retention window (no "please delete it manually" — removal is automatic).
	if isDataExportExpired(dataExport) {
		log.Printf("Case 2: DataExport idle TTL expired")
		readyCond := meta.FindStatusCondition(dataExport.Status.Conditions, string(ConditionReady))
		// Guard against redundant condition updates: if Ready is already Expired, skip the
		// SetStatusCondition call to avoid a spurious lastTransitionTime bump on every reconcile.
		if readyCond == nil || readyCond.Reason != string(ReasonExpired) {
			meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
				Type:               string(ConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             string(ReasonExpired),
				Message:            "DataExport idle timeout expired",
				ObservedGeneration: dataExport.Generation,
			})
		}
		// Expired is a long-lived state (the CR is kept for the GC), so the teardown is gated on our
		// finalizer still being there: dropping it is what records that the restore is done.
		if ContainsString(dataExport.Finalizers, dev1alpha1.StorageManagerFinalizerName) {
			done, blocked, err := r.clearDataExportProviding(ctx, dataExport, generatedNames)
			switch {
			case err != nil:
				if errors.Is(err, ErrInvalidOriginalReclaimPolicy) {
					// PV was labeled as inconsistent (missing or corrupted original reclaimPolicy).
					// Stop reconcile without error or requeue — finalizer stays, admin must investigate.
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, fmt.Errorf("%w: failed to restore configuration before DE: %w", ErrCleanupFailed, err)
			case blocked != nil:
				log.Printf("DataExport %s/%s: expiry teardown held by %s", req.Namespace, req.Name, blocked)
				setCleanupBlocked(dataExport, blocked)
				return ctrl.Result{RequeueAfter: dataExportRequeueInterval}, nil
			case !done:
				return ctrl.Result{RequeueAfter: dataExportRequeueInterval}, nil
			}
			RemoveFinalizer(ctx, r.Client, dataExport, dev1alpha1.StorageManagerFinalizerName)
		}
		return ctrl.Result{}, nil
	}

	// One-shot terminal (Failed): a DataExport that terminally failed validation is never re-provisioned
	// (VMOP model). Keep the body inert so a later resync (e.g. the VirtualDisk CRD appearing) does not
	// run Case 4 and detach the user PVC under a terminal phase, and so the GC clock (completionTimestamp,
	// stamped once) is not disturbed. Expired is handled by Case 2 above; a DataExport has no Completed
	// phase, so this only catches Failed.
	if Phase(dataExport.Status.Phase).IsTerminal() {
		log.Printf("DataExport %s/%s is terminal (phase=%s), skipping reconcile", req.Namespace, req.Name, dataExport.Status.Phase)
		return ctrl.Result{}, nil
	}

	// Case 3: Newly create DataExport resource (has no Condition with type Ready)
	readyCond := meta.FindStatusCondition(dataExport.Status.Conditions, string(ConditionReady))
	switch {
	case readyCond == nil:
		log.Printf("Case 3: DataExport resource newly created")
		// Initialize the Ready condition so downstream cases (Expired check, mutateReadyByErr) always have
		// a defined state to work with. The finalizer is added here to ensure clearDataExportProviding
		// runs on deletion even if the controller restarts before implementation is complete.
		meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
			Type:               string(ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(ReasonPending),
			Message:            "Started",
			ObservedGeneration: dataExport.Generation,
		})
		EnsureFinalizer(ctx, r.Client, dataExport, dev1alpha1.StorageManagerFinalizerName)
	// Case 4: DataExport resource needs to initial or continue implementation
	case readyCond.Status != metav1.ConditionTrue:
		log.Printf("Case 4: DataExport resource needs to initial or continue implementation")
		err = r.implementDataExportProviding(ctx, dataExport, generatedNames)
		if err != nil {
			if errors.Is(err, errTakeoverNotHealthy) {
				return ctrl.Result{RequeueAfter: dataExportRequeueInterval}, nil
			}
			if errors.Is(err, ErrPVCValidationFailed) {
				meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
					Type:               string(ConditionReady),
					Status:             metav1.ConditionFalse,
					Reason:             string(ReasonValidationFailed),
					Message:            err.Error(),
					ObservedGeneration: dataExport.Generation,
				})
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			return ctrl.Result{}, err
		}

	// Case 5: DataExport resource already implemented
	default:
		if err := r.reconcilePodReadyResources(ctx, dataExport, generatedNames); err != nil {
			if errors.Is(err, errTakeoverNotHealthy) {
				return ctrl.Result{RequeueAfter: dataExportRequeueInterval}, nil
			}
			return ctrl.Result{}, err
		}
		log.Printf("Case 5: DataExport resource providing already implemented")
	}

	return ctrl.Result{}, nil
}

// reconcileManagedResourceRecovery drives an object whose status.cleanupReason is set: it restores the
// user's volume, tears the export infrastructure down and only then lets the object settle as Failed
// (clearing the discriminator in the same status write that stamps the phase).
//
// Entry is keyed on the discriminator alone, never on the Ready reason. A blocked cleanup that carries no
// discriminator means the controller could not prove a safe target at all; it must stay out of here
// rather than have the barriers decide whether to proceed with an unproven one.
func (r *DataexportReconciler) reconcileManagedResourceRecovery(ctx context.Context, dataExport *dev1alpha1.DataExport) (ctrl.Result, error) {
	_, targetKindShort, err := classifyTargetRef(dataExport.Spec.TargetRef.Group, dataExport.Spec.TargetRef.Kind)
	if err != nil {
		// The spec turned invalid after the takeover. The recorded identity, not the spec, says what has
		// to be given back, so this must not strand the volume — but naming needs the kind, so fall back
		// to the kind the takeover was made with.
		targetKindShort = dev1alpha1.KindPVCShort
	}
	names := NewNamesFromShort(targetKindShort, dataExport.Spec.TargetRef.Name, dataExport.Namespace, dataExport.Name)

	done, blocked, err := r.clearDataExportProviding(ctx, dataExport, names)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("%w: managed-resource recovery: %w", ErrCleanupFailed, err)
	}
	if blocked != nil {
		log.Printf("DataExport %s/%s: recovery blocked by %s", dataExport.Namespace, dataExport.Name, blocked)
		setCleanupBlocked(dataExport, blocked)
		return ctrl.Result{RequeueAfter: dataExportRequeueInterval}, nil
	}
	if !done {
		return ctrl.Result{RequeueAfter: dataExportRequeueInterval}, nil
	}

	// The recovery finished. Restating the failure that caused it and clearing the discriminator happen in
	// the same in-memory mutation, so the object never appears with one and not the other: an empty
	// discriminator next to a managed-resource failure reason is exactly what tells the phase computation
	// that the mandatory restore is behind us.
	log.Printf("DataExport %s/%s: managed-resource recovery finished (%s)",
		dataExport.Namespace, dataExport.Name, dataExport.Status.CleanupReason)
	meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
		Type:               string(ConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             string(recoveryOutcomeReason(CleanupReason(dataExport.Status.CleanupReason))),
		Message:            recoveryOutcomeMessage(CleanupReason(dataExport.Status.CleanupReason)),
		ObservedGeneration: dataExport.Generation,
	})
	dataExport.Status.CleanupReason = ""
	dataExport.Status.Recovery = nil
	return ctrl.Result{}, nil
}

// setCleanupBlocked reports an unmet barrier on the object. Every path that runs the teardown says it the
// same way, because from the outside they are the same situation: the controller knows what it owes and
// cannot safely do it yet.
func setCleanupBlocked(dataExport *dev1alpha1.DataExport, blocked *recoveryBarrier) {
	meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
		Type:               string(ConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             string(ReasonCleanupBlocked),
		Message:            blocked.String(),
		ObservedGeneration: dataExport.Generation,
	})
}

// recoveryOutcomeReason maps the discriminator back to the failure it was set for, so the settled object
// keeps saying what went wrong rather than ending on the barrier chatter of the last blocked pass.
func recoveryOutcomeReason(cleanupReason CleanupReason) ConditionReason {
	if cleanupReason == CleanupReasonExportPVCIdentityMismatch {
		return ReasonManagedResourceIdentityMismatch
	}
	return ReasonManagedResourceLost
}

func recoveryOutcomeMessage(cleanupReason CleanupReason) string {
	if cleanupReason == CleanupReasonExportPVCIdentityMismatch {
		return "the export claim was replaced; the volume has been returned to its owner and the export was torn down"
	}
	return "the export claim was lost; the volume has been returned to its owner and the export was torn down"
}

// mutateReadyByErr maps known reconcile errors to Ready condition reasons.
// Called in defer after the reconcile body — translates sentinel errors (e.g. ErrTargetNotFound)
// into user-visible condition reasons without persisting; the caller (updateDataExport) handles persistence.
func mutateReadyByErr(dataExport *dev1alpha1.DataExport, reconcileErr error) {
	if reconcileErr == nil || dataExport == nil {
		return
	}

	// Keep terminal status stable in TTL/deletion cleanup flows.
	// Otherwise Ready can oscillate between Expired and CleanupFailed on each reconcile.
	if errors.Is(reconcileErr, ErrCleanupFailed) {
		readyCond := meta.FindStatusCondition(dataExport.Status.Conditions, string(ConditionReady))
		if readyCond != nil &&
			(readyCond.Reason == string(ReasonExpired) || readyCond.Reason == string(ReasonDeleted)) {
			return
		}
	}

	var reason ConditionReason
	switch {
	case errors.Is(reconcileErr, ErrCleanupFailed):
		reason = ReasonCleanupFailed
	case errors.Is(reconcileErr, ErrTargetNotFound):
		reason = ReasonTargetNotFound
	case errors.Is(reconcileErr, ErrPVConflict):
		reason = ReasonPVConflict
	case errors.Is(reconcileErr, ErrTargetNotReady):
		reason = ReasonTargetNotReady
	case errors.Is(reconcileErr, ErrDeploymentFailed):
		reason = ReasonDeploymentFailed
	default:
		// Keep status unchanged for unknown/transient errors.
		return
	}

	meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
		Type:               string(ConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             string(reason),
		Message:            reconcileErr.Error(),
		ObservedGeneration: dataExport.Generation,
	})
}

// isDataExportExpired reports whether the export has terminally idle-expired: the exporter pod reported
// serverState=IdleExpired, or a legacy object was expired under the old condition-based mechanism
// (Ready=False/Expired). Recognizing the legacy form keeps a migrated pre-upgrade expired object terminal
// instead of re-provisioning it (which would re-detach the user PVC).
func isDataExportExpired(de *dev1alpha1.DataExport) bool {
	if ServerState(de.Status.ServerState) == ServerStateIdleExpired {
		return true
	}
	ready := meta.FindStatusCondition(de.Status.Conditions, string(ConditionReady))
	return ready != nil && ready.Status == metav1.ConditionFalse && ready.Reason == string(ReasonExpired)
}

// computeDataExportPhase derives the coarse-grained phase (a DataExport has no Completed phase). A
// terminal phase is STICKY, and terminal transitions are EXPLICIT: Failed only from an ErrTerminal
// reconcile error (never from an ambiguous condition reason like ValidationFailed that is also used for
// retryable failures) or from a finished managed-resource recovery, Expired from the idle signal.
// Precedence: deletion > owed recovery > sticky-terminal > expired > failed > ready > pending.
func computeDataExportPhase(de *dev1alpha1.DataExport, reconcileErr error) Phase {
	if de.DeletionTimestamp != nil {
		return PhaseTerminating
	}
	// An object that still owes a managed-resource recovery must not be terminal, because terminal means
	// reapable: the garbage collector would delete the DataExport while the PV is still bound to a claim
	// the user does not own, and the only cheap entry point into the recovery context would go with it.
	// This outranks stickiness on purpose — a settled terminal phase found next to a non-empty
	// discriminator is not a legal state, and un-sticking is the safe direction (the recovery branch
	// re-stamps the terminal phase when it finishes).
	if de.Status.CleanupReason != "" {
		return PhasePending
	}
	if Phase(de.Status.Phase).IsTerminal() {
		return Phase(de.Status.Phase)
	}
	if isDataExportExpired(de) {
		return PhaseExpired
	}
	if errors.Is(reconcileErr, ErrTerminal) {
		return PhaseFailed
	}
	// Recovery finished: an empty discriminator next to a managed-resource failure reason is the
	// persisted proof that the mandatory restore already ran, so the operation may now settle as Failed.
	// This needs no ErrTerminal, because the last recovery pass returns success.
	if ready := meta.FindStatusCondition(de.Status.Conditions, string(ConditionReady)); ready != nil &&
		ready.Status == metav1.ConditionFalse && IsManagedResourceFailureReason(ConditionReason(ready.Reason)) {
		return PhaseFailed
	}
	if meta.IsStatusConditionTrue(de.Status.Conditions, string(ConditionReady)) {
		return PhaseReady
	}
	return PhasePending
}

// finalizeDataExportStatus computes phase and stamps completionTimestamp once. It runs in the deferred
// block after the reconcile body and mutateReadyByErr, so it always sees the definitive Ready condition
// (which the reconcile body already sets: Expired in Case 2, ValidationFailed on terminal errors). The
// controller is the sole writer of phase, completionTimestamp and the Ready condition.
func (r *DataexportReconciler) finalizeDataExportStatus(de *dev1alpha1.DataExport, reconcileErr error) {
	if de == nil {
		return
	}
	phase := computeDataExportPhase(de, reconcileErr)
	de.Status.Phase = string(phase)
	SetCompletionTimestampOnce(&de.Status, phase, r.now())
}

// updateDataExport persists the accumulated in-memory mutations (spec/metadata, status) at the end of
// Reconcile. Metadata and the status subresource are independent resources, so each gets its own API
// call. Both are wrapped in RetryOnConflict with a fresh GET: the exporter pod writes the pod-owned
// status fields (serverState, accessTimestamp, url, ca) concurrently (heartbeat every 30s), so a blind
// Status().Update off the reconcile-start snapshot would clobber a concurrent serverState=IdleExpired
// write. Instead the controller-owned status is re-applied onto the freshly read object while the
// pod-owned fields are carried over from the latest server-side state.
func (r *DataexportReconciler) updateDataExport(ctx context.Context, dataExportOld, dataExportNew *dev1alpha1.DataExport) error {
	if dataExportNew == nil || dataExportOld == nil {
		return nil
	}

	// The controller only ever changes its own finalizer (and status); it never writes spec/labels/
	// annotations. So the metadata write is reduced to reconciling that single finalizer onto the fresh
	// object, which avoids clobbering concurrent third-party edits (spec, labels, foregroundDeletion).
	// clearDataExportProviding zeroes Finalizers, so a removal here strips exactly our finalizer.
	wantFinalizer := ContainsString(dataExportNew.Finalizers, dev1alpha1.StorageManagerFinalizerName)
	needMeta := wantFinalizer != ContainsString(dataExportOld.Finalizers, dev1alpha1.StorageManagerFinalizerName)
	needStatus := !reflect.DeepEqual(dataExportOld.Status, dataExportNew.Status)
	if !needMeta && !needStatus {
		return nil
	}

	statusCopy := dataExportNew.Status.DeepCopy()
	key := types.NamespacedName{Namespace: dataExportNew.Namespace, Name: dataExportNew.Name}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &dev1alpha1.DataExport{}
		if err := r.Client.Get(ctx, key, fresh); err != nil {
			return err
		}

		if needMeta {
			fresh.Finalizers = ReconcileFinalizer(fresh.Finalizers, dev1alpha1.StorageManagerFinalizerName, wantFinalizer)
			if err := r.Client.Update(ctx, fresh); err != nil {
				return fmt.Errorf("update DataExport failed: %w", err)
			}
		}

		if needStatus {
			merged := statusCopy.DeepCopy()
			// Preserve the pod-owned status fields from the latest server-side object; the controller owns
			// everything else (the Ready condition, phase, completionTimestamp, publicURL, volumeMode).
			merged.Url = fresh.Status.Url
			merged.CA = fresh.Status.CA
			merged.AccessTimestamp = fresh.Status.AccessTimestamp
			merged.ServerState = fresh.Status.ServerState
			fresh.Status = *merged
			if err := r.Client.Status().Update(ctx, fresh); err != nil {
				return fmt.Errorf("update DataExport status failed: %w", err)
			}
		}

		return nil
	})
	// The object was deleted out from under us (finalizer just removed and Kubernetes finalized deletion,
	// or the GC deleted it): nothing left to persist — treat as success.
	return client.IgnoreNotFound(err)
}

func (r *DataexportReconciler) implementDataExportProviding(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames Names) error {
	log.Printf("Start realizing DE for resource %s, userPVC: %s, ttl: %s", dataExport.Name, dataExport.Spec.TargetRef.Name, dataExport.Spec.Ttl)

	var exportPVC *corev1.PersistentVolumeClaim
	// pv is fetched once during exportPVC creation and passed through to ensureExportPVReady
	// to avoid a redundant Get call. It is nil when exportPVC already exists (idempotent restart),
	// in which case ensureExportPVReady fetches PV from exportPVC.Spec.VolumeName.
	var pv *corev1.PersistentVolume
	var userPVCName string

	var err error
	exportPVC, err = r.validateExportPVC(ctx, dataExport, generatedNames.ExportPVCName)
	if err != nil {
		return err
	}

	// A missing export claim is only re-creatable while the volume has not changed hands yet; afterwards
	// re-creating it is impossible and provisioning would take the volume over a second time.
	state, err := r.resolveTakeoverState(ctx, dataExport, generatedNames, exportPVC)
	if err != nil {
		return err
	}
	if state.kind != takeoverHealthy {
		log.Printf("DataExport %s/%s: %s", dataExport.Namespace, dataExport.Name, state.message)
		applyManagedResourceFailure(dataExport, state)
		return errTakeoverNotHealthy
	}

	if exportPVC == nil {
		log.Printf("Export PVC %s not found for DataExport resource %s, creating new one", generatedNames.ExportPVCName, dataExport.Name)

		switch generatedNames.TargetKindShort {
		case dev1alpha1.KindPVCShort:
			log.Printf("Export target kind: %s", generatedNames.TargetKindShort)
			userPVCName = dataExport.Spec.TargetRef.Name
			exportPVC, pv, err = r.getExportPVCFromUserPVC(ctx, dataExport.Namespace, userPVCName, generatedNames.ExportPVCName, dataExport)
			if err != nil {
				return fmt.Errorf("failed to process user PVC export: %w", err)
			}
		case dev1alpha1.KindVirtualDiskShort:
			log.Printf("Export target kind: %s", generatedNames.TargetKindShort)
			exportPVC, pv, userPVCName, err = r.getExportPVCFromUserVirtualDisk(ctx, dataExport, generatedNames)
			if err != nil {
				return fmt.Errorf("failed to process user VirtualDisk export: %w", err)
			}

		case dev1alpha1.KindSnapshotShort:
			// Resource-agnostic snapshot path (C6): any namespaced snapshot leaf (generic VolumeSnapshot,
			// VirtualDiskSnapshot, domain snapshot, ...) is exported the same way — resolve the leaf's
			// SnapshotContent.dataRef and provision the export PVC from the durable artifact via a VRR.
			log.Printf("Export target kind: %s", generatedNames.TargetKindShort)
			exportPVC, err = r.getExportPVCFromSnapshot(ctx, dataExport, generatedNames)
			if err != nil {
				return fmt.Errorf("failed to process snapshot export: %w", err)
			}
		default:
			return fmt.Errorf("unknown export kind: %s", generatedNames.TargetKindShort)
		}

		log.Printf("Export PVC %s created for DataExport resource %s", exportPVC.GetName(), dataExport.Name)
	}

	// TODO: refactor this
	if userPVCName == "" {
		userPVCName, err = r.resolveUserPVCName(ctx, dataExport, generatedNames.TargetKindShort)
		if err != nil {
			return fmt.Errorf("failed to resolve user PVC name: %w", err)
		}
	}

	// Ensure PV has all required annotations/labels for orphan recovery.
	// This is needed for idempotency if controller restarts after exportPVC creation
	// but before PV was patched with tracking annotations.
	// pv may be nil if exportPVC already existed (idempotent restart).
	// ensureExportPVReady handles this case by fetching PV from exportPVC.Spec.VolumeName.
	if err := r.ensureExportPVReady(ctx, pv, exportPVC, generatedNames, dataExport, userPVCName); err != nil {
		return err
	}

	// On an idempotent restart the export PVC comes from cache (validateExportPVC) without a VolumeMode
	// nil-check; for the snapshot path it is provisioned out-of-band by the external-provisioner, so guard
	// against a not-yet-shaped PVC and requeue instead of panicking.
	if exportPVC.Spec.VolumeMode == nil {
		return fmt.Errorf("export PVC %s has no volumeMode yet: %w", exportPVC.Name, ErrTargetNotReady)
	}
	dataExport.Status.VolumeMode = string(*exportPVC.Spec.VolumeMode)

	// create export deployment and wait for running

	var exportDeploy *appsv1.Deployment
	exportDeploy, err = r.validateExportDeploy(ctx, dataExport, generatedNames.DeployName)
	if err != nil {
		return err
	}
	if exportDeploy == nil {
		err = r.createDeployment(ctx, dataExport, exportPVC, generatedNames)
		if err != nil {
			return err
		}
	}

	if err := r.reconcilePublishResources(ctx, dataExport, generatedNames); err != nil {
		return err
	}

	return nil
}

// reconcilePodReadyResources is invoked on every reconcile iteration for a fully
// provisioned DataExport (Case 5) to keep ancillary resources in sync with the
// current spec. It re-evaluates the Publish toggle so that enabling or disabling
// external access takes effect without re-creating the DataExport object.
func (r *DataexportReconciler) reconcilePodReadyResources(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames Names) error {
	// Drift repair: if the export Deployment was deleted mid-life (Case 5, Ready=True), downgrade readiness
	// so the next reconcile recreates it via Case 4. implementDataExportProviding reuses the still-present
	// export PVC (validateExportPVC returns it, so the user-PVC detach is NOT repeated) and recreates only
	// the missing Deployment. The phase goes Ready->Pending, never to a terminal outcome: downtime does not
	// burn the idle TTL, because the recreated exporter pod restarts its idle timer from scratch.
	//
	// Scope: only the Deployment is repaired here. The export PVC is NOT repairable — once the user PV has
	// been rebound onto it, that claim cannot be recreated (the binding pins the deleted claim's UID), so
	// its loss is a failure to recover from rather than drift to fix, and it is classified below before
	// anything else runs. The CA Secret is (re)created by the exporter pod on start, so its loss is
	// repaired by the pod once the Deployment exists.
	//
	// The claim is checked first: with the volume bound to a claim that no longer exists, the export
	// serves nothing, and recreating its Deployment would only rebuild a pod around a dead claim.
	exportPVC, err := r.getExportPVC(ctx, generatedNames.ExportPVCName)
	if err != nil {
		return err
	}
	state, err := r.resolveTakeoverState(ctx, dataExport, generatedNames, exportPVC)
	if err != nil {
		return err
	}
	if state.kind != takeoverHealthy {
		log.Printf("DataExport %s/%s: %s", dataExport.Namespace, dataExport.Name, state.message)
		applyManagedResourceFailure(dataExport, state)
		return errTakeoverNotHealthy
	}

	deploy, err := r.getServerDeployment(ctx, generatedNames.DeployName)
	if err != nil {
		return err
	}
	if deploy == nil {
		log.Printf("DataExport %s/%s server Deployment missing; re-provisioning", dataExport.Namespace, dataExport.Name)
		meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
			Type:               string(ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(ReasonPending),
			Message:            "Server Deployment missing; re-provisioning",
			ObservedGeneration: dataExport.Generation,
		})
		return nil
	}

	// One-time migration: stamp the owning-DataExport annotations onto a Deployment created before the
	// controller carried them on the Deployment object, so its watch maps a mid-life delete/change back to
	// this DataExport (createRequest reads these annotations) and drift-repair fires promptly rather than
	// only on the periodic resync.
	if err := r.ensureDeploymentTrackingAnnotations(ctx, deploy, dataExport); err != nil {
		return err
	}

	if err := r.reconcilePublishResources(ctx, dataExport, generatedNames); err != nil {
		return err
	}

	return nil
}

// getServerDeployment returns the export server Deployment, or (nil, nil) if it does not exist. It is a
// lightweight existence probe used for drift detection, without the blocking readiness poll of
// validateExportDeploy.
func (r *DataexportReconciler) getServerDeployment(ctx context.Context, deployName string) (*appsv1.Deployment, error) {
	deploy := &appsv1.Deployment{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: deployName}, deploy)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get export deployment %s: %w", deployName, err)
	}
	return deploy, nil
}

// ensureDeploymentTrackingAnnotations back-fills the owning-DataExport annotations on an export Deployment
// that predates the controller stamping them (upgrade migration), so its watch can map its events back to
// the DataExport. It is a no-op once the annotations are present.
func (r *DataexportReconciler) ensureDeploymentTrackingAnnotations(ctx context.Context, deploy *appsv1.Deployment, dataExport *dev1alpha1.DataExport) error {
	if deploy.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] == dataExport.Namespace &&
		deploy.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] == dataExport.Name {
		return nil
	}
	patched := deploy.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] = dataExport.Namespace
	patched.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] = dataExport.Name
	if err := r.Client.Patch(ctx, patched, client.MergeFrom(deploy)); err != nil {
		return fmt.Errorf("failed to stamp tracking annotations on export deployment %s: %w", deploy.Name, err)
	}
	return nil
}

// reconcilePublishResources creates or removes the public Service and Ingress
// based on dataExport.Spec.Publish, keeping dataExport.Status.PublicURL consistent.
// When Publish is true, EnsurePublicURL is called and the resulting URL is written
// to status (skipped if the URL has not changed). When Publish is false, both
// resources are deleted and the status URL is cleared.
func (r *DataexportReconciler) reconcilePublishResources(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames Names) error {
	serviceCfg, ingressCfg := r.makePublishConfigs(dataExport, generatedNames)

	if dataExport.Spec.Publish {
		// ensure Service and Ingress exist, get the resulting public URL
		publicURL, err := publish.EnsurePublicURL(ctx, r.Client, r.Reader, serviceCfg, ingressCfg)
		if err != nil {
			meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
				Type:               string(ConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             string(ReasonPublishFailed),
				Message:            err.Error(),
				ObservedGeneration: dataExport.Generation,
			})
			return fmt.Errorf("failed to ensure public URL: %w", err)
		}

		// skip status update if URL hasn't changed
		if dataExport.Status.PublicURL == publicURL {
			return nil
		}

		dataExport.Status.PublicURL = publicURL

		return nil
	}

	// Publish disabled: remove Service and Ingress.
	// DeletePublicResources is idempotent - not-found is not an error.
	if _, err := publish.DeletePublicResources(ctx, r.Client, serviceCfg.ServiceName, ingressCfg.IngressName); err != nil {
		return fmt.Errorf("failed to delete public resources: %w", err)
	}
	dataExport.Status.PublicURL = ""

	return nil
}

// resolveUserPVCName returns the actual user PVC name for the given DataExport.
// For PVC targets, TargetRef.Name is the PVC name directly.
// For VirtualDisk targets, TargetRef.Name is the VD name - the actual PVC name
// is resolved from VirtualDisk.Status.Target.PersistentVolumeClaim.
// For snapshot-based targets, returns empty string (no user PVC to protect).
func (r *DataexportReconciler) resolveUserPVCName(ctx context.Context, dataExport *dev1alpha1.DataExport, targetKindShort string) (string, error) {
	switch targetKindShort {
	case dev1alpha1.KindPVCShort:
		return dataExport.Spec.TargetRef.Name, nil
	case dev1alpha1.KindVirtualDiskShort:
		vd := &virtv1alpha2.VirtualDisk{}
		if err := r.Client.Get(ctx, types.NamespacedName{
			Namespace: dataExport.Namespace,
			Name:      dataExport.Spec.TargetRef.Name,
		}, vd); err != nil {
			return "", fmt.Errorf("failed to get VirtualDisk %s/%s: %w",
				dataExport.Namespace, dataExport.Spec.TargetRef.Name, err)
		}
		pvcName := vd.Status.Target.PersistentVolumeClaim
		if pvcName == "" {
			return "", fmt.Errorf("VirtualDisk %s/%s has no PVC name in status",
				dataExport.Namespace, dataExport.Spec.TargetRef.Name)
		}
		return pvcName, nil
	default:
		return "", nil
	}
}

func (r *DataexportReconciler) makePublishConfigs(dataExport *dev1alpha1.DataExport, generatedNames Names) (publish.HeadlessServiceCfg, publish.IngressCfg) {
	serviceCfg := publish.HeadlessServiceCfg{
		ServiceName:           types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: generatedNames.HeadlessServiceName},
		DeploymentName:        generatedNames.DeployName,
		LabelApplicationValue: dev1alpha1.LabelDataExportValue,
	}
	ingressCfg := publish.IngressCfg{
		IngressName:      types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: generatedNames.IngressResourceName},
		ServiceName:      types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: generatedNames.HeadlessServiceName},
		OriginIngress:    types.NamespacedName{Namespace: r.Config.OriginIngressNamespace, Name: OriginIngressName},
		TargetSecretName: IngressSecretName,
		Path:             fmt.Sprintf("/%s/%s/%s", dataExport.Namespace, generatedNames.TargetKindShort, generatedNames.TargetName),
		CorsAllowMethods: "GET, HEAD, OPTIONS",
	}

	return serviceCfg, ingressCfg
}

// ensureExportPVReady ensures that the PV backing the exportPVC has all required
// annotations and labels for orphan cleanup and tracking. This method provides
// idempotency when the controller restarts after exportPVC was created but before
// PV annotations were applied.
// If pv is nil (idempotent restart when exportPVC already exists), the PV is fetched
// from exportPVC.Spec.VolumeName.
// userPVCName is the resolved name of the actual user PVC (for VirtualDisk targets,
// this is the underlying PVC name from VD status, not the VD name).
func (r *DataexportReconciler) ensureExportPVReady(ctx context.Context, pv *corev1.PersistentVolume, exportPVC *corev1.PersistentVolumeClaim, generatedNames Names, dataExport *dev1alpha1.DataExport, userPVCName string) error {
	// Skip for snapshot-based exports (VolumeSnapshot, VirtualDiskSnapshot) because they
	// create new PVs via CSI provisioner and don't detach existing user PVCs.
	// TODO: For snapshot-based exports, add polling to wait until PV is provisioned by CSI driver.
	if generatedNames.TargetKindShort != dev1alpha1.KindPVCShort && generatedNames.TargetKindShort != dev1alpha1.KindVirtualDiskShort {
		return nil
	}

	// If PV was not passed (idempotent restart case), fetch it from exportPVC.
	if pv == nil {
		pvName := exportPVC.Spec.VolumeName
		if pvName == "" {
			return fmt.Errorf("export PVC has no volume name")
		}

		pv = &corev1.PersistentVolume{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
			if kubeerrors.IsNotFound(err) {
				return fmt.Errorf("export pvc volume %s not found", pvName)
			}

			log.Printf("Error getting PV %s: %v", pvName, err)
			return err
		}
	}

	userPVC, err := r.ensureUserPVCExportingAnnotationAndFinalizer(ctx, dataExport.Namespace, userPVCName)
	if err != nil {
		return err
	}

	// Check if PV already has all required annotations with correct values.
	// The parsed result is not needed - only validation matters
	_, err = parsePVRecoveryInfo(pv, pvOwnerExpectation{
		DataExportNamespace: dataExport.Namespace,
		DataExportName:      dataExport.Name,
		DataExportUID:       dataExport.UID,
		SourcePVCUID:        userPVC.UID,
	})
	// An identity conflict is not drift to be patched over: re-annotating the PV would erase the record
	// of the takeover that is actually in force. Report it and leave both the PV and our status alone —
	// recording the live identity here would put into status exactly the takeover the PV just rejected.
	if errors.Is(err, ErrPVConflict) {
		return err
	}
	infoIsValid := err == nil

	// Record who the volume is being taken from before anything is taken. This runs on every pass, not
	// only on the one that patches the PV, so a controller that died between the patch and the status
	// write repairs the record instead of losing it. A legacy takeover cannot be recorded at all — see
	// takeoverIdentityIsProvable.
	identityProvable := takeoverIdentityIsProvable(pv, userPVC)
	if identityProvable {
		if err := recordTakeoverIdentity(dataExport, pv, exportPVC, userPVC); err != nil {
			return err
		}
	} else {
		log.Printf("PV %s carries a pre-UID takeover; running without a recorded identity", pv.Name)
	}

	// PV spec must also be in export-ready state:
	// ReclaimPolicy=Retain protects data, ClaimRef binds PV to exportPVC.
	specReady := pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain &&
		pv.Spec.ClaimRef != nil &&
		pv.Spec.ClaimRef.Name == exportPVC.Name &&
		pv.Spec.ClaimRef.Namespace == exportPVC.Namespace

	// If both annotations and spec are already correct, skip patching
	if infoIsValid && specReady {
		return nil
	}

	if err := r.patchPVLabelAnnotationsClaimRef(ctx, pv, exportPVC, dataExport, generatedNames, userPVC, identityProvable); err != nil {
		log.Printf("failed to patch PV %s: %v", pv.Name, err)
		return err
	}

	return nil
}

// rebindIdentityExpectation says what the caller of a rebind can prove about the takeover it is undoing.
// An empty DataExportUID means it cannot prove one: the orphan sweep starts from a parent that is already
// gone, so the recorded export UID is read but not judged. Making that a parameter keeps the limits of
// each path visible at its call site instead of resting on what some earlier call happened to check.
type rebindIdentityExpectation struct {
	DataExportUID types.UID
}

// checkRebindIdentity decides whether the volume may be handed back to the claim found by name:
//
//	identity recorded and matching what the caller can prove -> rebind
//	recorded source claim UID differs from the live claim    -> refuse; that is a different owner
//	recorded export UID differs from a provable expectation  -> refuse; that is a different export
//	exactly one annotation recorded                          -> refuse; half an identity is corruption
//	neither recorded                                         -> rebind on the legacy namespace/name contract
//
// The last case is a deliberate backward-compatibility concession, not the general recovery contract. A
// takeover from before the identity model has no UID to offer and its parent may already be gone, so
// refusing would turn the upgrade into a new class of stranded volumes with no automatic way out.
// Missing evidence is therefore tolerated only for objects the current code can no longer create;
// evidence that contradicts what the caller can prove always blocks the rebind.
func checkRebindIdentity(pv *corev1.PersistentVolume, userPVC *corev1.PersistentVolumeClaim, expect rebindIdentityExpectation) error {
	recordedClaimUID := pv.Annotations[dev1alpha1.AnnotationUserPVCUIDKey]
	recordedExportUID := pv.Annotations[dev1alpha1.AnnotationDataExportUIDKey]

	switch {
	case (recordedClaimUID == "") != (recordedExportUID == ""):
		return fmt.Errorf("PV %s has an incomplete takeover identity (%s=%q, %s=%q) and cannot be rebound to %s/%s: %w",
			pv.Name,
			dev1alpha1.AnnotationDataExportUIDKey, recordedExportUID,
			dev1alpha1.AnnotationUserPVCUIDKey, recordedClaimUID,
			userPVC.Namespace, userPVC.Name, ErrPVConflict)

	case recordedClaimUID != "" && recordedClaimUID != string(userPVC.UID):
		return fmt.Errorf("PV %s was taken from claim %s/%s with UID %s, but the live claim has UID %s: %w",
			pv.Name, userPVC.Namespace, userPVC.Name, recordedClaimUID, userPVC.UID, ErrPVConflict)

	// Only a recorded value can contradict the expectation; an absent one means legacy, handled below.
	case recordedExportUID != "" && expect.DataExportUID != "" && recordedExportUID != string(expect.DataExportUID):
		return fmt.Errorf("PV %s was taken over by DataExport %s, but the export undoing the takeover is %s: %w",
			pv.Name, recordedExportUID, expect.DataExportUID, ErrPVConflict)

	case recordedClaimUID == "":
		log.Printf("LegacyIdentityFallback: PV %s records no takeover identity; rebinding to claim %s/%s (UID %s) on the pre-UID namespace/name contract",
			pv.Name, userPVC.Namespace, userPVC.Name, userPVC.UID)
	}

	return nil
}

// takeoverIdentityIsProvable answers whether the controller can show that the claim it is holding is the
// one the volume belongs to. There are exactly two proofs:
//
//   - the PV already records the identity, which parsePVRecoveryInfo has just matched against the live
//     objects (a mismatch, or half a pair, never gets this far);
//   - the PV is still bound to that exact claim, so this pass is the takeover itself.
//
// A claim that merely names the volume is not a proof: a PV can lose its claimRef and be named again by
// a recreated claim, and treating that as ownership would turn an unproven legacy state into a recorded
// one. An unbound PV therefore counts as unprovable, which is why the identity is written before the
// takeover repoints claimRef, never after.
//
// Everything else is a takeover from before the UID model: the PV is already held by the export claim and
// nothing but a name connects it to the claim in hand. Recording an identity there would manufacture the
// very evidence recovery is supposed to verify, so such an export runs on with none. Takeovers the
// current code performs therefore always carry a proven identity, while pre-UID ones stay explicitly
// eligible for the bounded legacy fallback in checkRebindIdentity.
func takeoverIdentityIsProvable(pv *corev1.PersistentVolume, userPVC *corev1.PersistentVolumeClaim) bool {
	if pv.Annotations[dev1alpha1.AnnotationUserPVCUIDKey] != "" {
		return true
	}
	claimRef := pv.Spec.ClaimRef
	return claimRef != nil && claimRef.UID != "" && claimRef.UID == userPVC.UID
}

// recordTakeoverIdentity pins, in the export's own status, which objects this export borrowed the volume
// from. Namespace/name are not enough to give it back: a recreated PVC reuses the name under a fresh UID,
// and rebinding a PV to the wrong claim of the right name hands a user someone else's data.
//
// The record is write-once. Refreshing it from whatever is live now would make every later comparison
// agree with itself, which is precisely the check that has to fail when a claim was replaced. A
// disagreement is therefore reported as a conflict; resolving it is the recovery path's job.
func recordTakeoverIdentity(dataExport *dev1alpha1.DataExport, pv *corev1.PersistentVolume, exportPVC, userPVC *corev1.PersistentVolumeClaim) error {
	recorded := dev1alpha1.RecoveryStatus{
		SourcePVCUID: string(userPVC.UID),
		ExportPVCUID: string(exportPVC.UID),
		PVName:       pv.Name,
		PVUID:        string(pv.UID),
	}

	if existing := dataExport.Status.Recovery; existing != nil {
		if *existing != recorded {
			return fmt.Errorf("DataExport %s/%s recorded a different takeover (%+v, live %+v): %w",
				dataExport.Namespace, dataExport.Name, *existing, recorded, ErrPVConflict)
		}
		return nil
	}

	dataExport.Status.Recovery = &recorded
	return nil
}

func (r *DataexportReconciler) detachPVC(ctx context.Context, userPVC *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, exportPVCName string, dataExport *dev1alpha1.DataExport) (*corev1.PersistentVolumeClaim, error) {
	// Check for conflicts before modifying PV
	if err := validatePVNotOwnedByAnotherDataExport(pv, userPVC.Namespace, dataExport.Name); err != nil {
		return nil, err
	}

	log.Printf("Detaching PVC %s/%s from PV %s", userPVC.Namespace, userPVC.Name, pv.Name)
	// Temporary PVC for dataExport pod
	exportPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      exportPVCName,
			Namespace: r.Config.ControllerNamespace,
			Labels: map[string]string{
				dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue,
			},
			// Creation is the only moment this claim's origin is known first-hand, and the marker is what
			// a later pass has instead: the name is derived from the export and therefore says nothing
			// about who occupies it. The parent UID is what makes the proof exclusive — namespace and name
			// are reused by a recreated DataExport, a UID never is.
			Annotations: map[string]string{
				dev1alpha1.AnnotationDataExportUIDKey:           string(dataExport.UID),
				dev1alpha1.AnnotationStorageManagerNamespaceKey: dataExport.Namespace,
				dev1alpha1.AnnotationStorageManagerNameKey:      dataExport.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:       pv.GetName(),
			VolumeMode:       userPVC.Spec.VolumeMode,
			StorageClassName: userPVC.Spec.StorageClassName,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
			Resources:        userPVC.Spec.Resources,
		},
	}

	log.Printf("Creating export PVC %s/%s for user PVC %s/%s", exportPVC.Namespace, exportPVC.Name, userPVC.Namespace, userPVC.Name)
	err := r.Client.Create(ctx, exportPVC)
	if err != nil {
		return nil, fmt.Errorf("failed to create export PVC: %w", err)
	}

	return exportPVC, nil
}

// patchPVLabelAnnotationsClaimRef patches the PV with:
// - ClaimRef pointing to exportPVC (to bind PV to export PVC)
// - Annotations for tracking original userPVC, DataExport name, and original reclaimPolicy
// - Label for cache filtering (only labeled PVs are cached) and orphan PV discovery
// - ReclaimPolicy set to Retain to protect data if exportPVC is accidentally deleted
func (r *DataexportReconciler) patchPVLabelAnnotationsClaimRef(ctx context.Context, pv *corev1.PersistentVolume, exportPVC *corev1.PersistentVolumeClaim, dataExport *dev1alpha1.DataExport, names Names, userPVC *corev1.PersistentVolumeClaim, identityProvable bool) error {
	updatedPv := pv.DeepCopy()

	if len(updatedPv.Annotations) == 0 {
		updatedPv.Annotations = make(map[string]string)
	}

	if len(updatedPv.Labels) == 0 {
		updatedPv.Labels = make(map[string]string)
	}

	updatedPv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace:       exportPVC.Namespace,
		Name:            exportPVC.Name,
		UID:             exportPVC.UID,
		ResourceVersion: exportPVC.ResourceVersion,
	}

	// Used for tracking original userPVC
	updatedPv.Annotations[dev1alpha1.AnnotationUserPVCNamespaceKey] = dataExport.Namespace
	updatedPv.Annotations[dev1alpha1.AnnotationUserPVCNameKey] = userPVC.Name

	// This used for exportRequest func
	updatedPv.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] = dataExport.Namespace
	updatedPv.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] = dataExport.Name

	// The takeover identity, kept on the PV as well as in the export status: the PV outlives its claims
	// and is the one object still present when either claim is gone. A repair pass over a legacy
	// takeover has nothing to prove the source claim with, and must leave the pair absent rather than
	// stamp the name-resolved claim as if it had been verified.
	if identityProvable {
		updatedPv.Annotations[dev1alpha1.AnnotationDataExportUIDKey] = string(dataExport.UID)
		updatedPv.Annotations[dev1alpha1.AnnotationUserPVCUIDKey] = string(userPVC.UID)
	}

	// This used for removing orphan resources
	updatedPv.Annotations[dev1alpha1.AnnotationPVTargetKindShortKey] = names.TargetKindShort
	updatedPv.Annotations[dev1alpha1.AnnotationPVHashSuffixKey] = names.HashSuffix

	// Save original reclaimPolicy before changing it
	updatedPv.Annotations[dev1alpha1.AnnotationOriginalReclaimPolicyKey] = string(pv.Spec.PersistentVolumeReclaimPolicy)

	// Set Retain to protect data during export
	updatedPv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain

	// Add labels for efficient List queries with MatchingLabels
	updatedPv.Labels[dev1alpha1.LabelPVDataExporter] = "true"

	log.Printf("Patching PV %s for attach to export PVC %s/%s", pv.Name, exportPVC.Namespace, exportPVC.Name)
	err := r.Client.Patch(ctx, updatedPv, client.MergeFromWithOptions(pv, client.MergeFromWithOptimisticLock{}))
	if err != nil {
		return fmt.Errorf("failed to patch user PV: %w", err)
	}

	return nil
}

var ErrInvalidOriginalReclaimPolicy = errors.New("invalid original reclaim policy")

// parsePVRecoveryInfo reads and validates all data-export annotations from a PV.
// Returns error if any required annotation is missing, has an invalid value,
// or doesn't match the expected DataExport (namespace/name, and UIDs when the caller knows them).
func parsePVRecoveryInfo(pv *corev1.PersistentVolume, expect pvOwnerExpectation) (*pvRecoveryInfo, error) {
	deNS, deName := expect.DataExportNamespace, expect.DataExportName
	// No check userPVC name, because it can be empty (snapshot-based case)
	userPVCName := pv.Annotations[dev1alpha1.AnnotationUserPVCNameKey]
	originalReclaimPolicy := corev1.PersistentVolumeReclaimPolicy(pv.Annotations[dev1alpha1.AnnotationOriginalReclaimPolicyKey])

	// Check valid reclaim policy from annotations
	// Validate originalReclaimPolicy first: this is the only annotation whose absence
	// makes recovery impossible (we cannot restore PV in normal flow (clearDataExportProviding)
	// without knowing the original policy). ErrInvalidOriginalReclaimPolicy is handled specially
	// by callers to stop cleanup and label the PV as inconsistent, while other validation
	// errors allow recovery to proceed.
	switch originalReclaimPolicy {
	case corev1.PersistentVolumeReclaimRetain,
		corev1.PersistentVolumeReclaimDelete,
		corev1.PersistentVolumeReclaimRecycle:
		// Valid policy
	default:
		return nil, fmt.Errorf("%w: invalid PV reclaim policy: %v", ErrInvalidOriginalReclaimPolicy, originalReclaimPolicy)
	}

	if pv.Labels[dev1alpha1.LabelPVDataExporter] != "true" {
		return nil, fmt.Errorf("PV %s has invalid label %s", pv.Name, dev1alpha1.LabelPVDataExporter)
	}

	dataExportName := pv.Annotations[dev1alpha1.AnnotationStorageManagerNameKey]
	if dataExportName != deName {
		return nil, fmt.Errorf("PV %s has invalid annotation %s", pv.Name, dev1alpha1.AnnotationStorageManagerNameKey)
	}

	dataExportNamespace := pv.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey]
	if dataExportNamespace != deNS {
		return nil, fmt.Errorf("PV %s has invalid annotation %s", pv.Name, dev1alpha1.AnnotationStorageManagerNamespaceKey)
	}

	userPVCNamespace := pv.Annotations[dev1alpha1.AnnotationUserPVCNamespaceKey]
	if userPVCNamespace != deNS {
		return nil, fmt.Errorf("PV %s has invalid annotation %s", pv.Name, dev1alpha1.AnnotationUserPVCNamespaceKey)
	}

	targetKindShort := pv.Annotations[dev1alpha1.AnnotationPVTargetKindShortKey]
	hashSuffix := pv.Annotations[dev1alpha1.AnnotationPVHashSuffixKey]
	if err := ValidateHashAndTarget(targetKindShort, hashSuffix, deNS, deName); err != nil {
		return nil, err
	}

	// The UID pair distinguishes the objects a name cannot: a DataExport recreated under the same name is
	// a different owner, and a user PVC recreated under the same name is a different claim. Both are
	// checked only when the annotation is present (a legacy takeover has none) and the caller can prove
	// what it expects.
	dataExportUID := pv.Annotations[dev1alpha1.AnnotationDataExportUIDKey]
	userPVCUID := pv.Annotations[dev1alpha1.AnnotationUserPVCUIDKey]

	// Half a pair is not a legacy takeover: whoever wrote one annotation knew the UID model, so the
	// other one was lost. Letting it through the legacy door would silently downgrade a corrupted
	// takeover to an unverified one.
	if (dataExportUID == "") != (userPVCUID == "") {
		return nil, fmt.Errorf("PV %s has an incomplete takeover identity (%s=%q, %s=%q): %w",
			pv.Name,
			dev1alpha1.AnnotationDataExportUIDKey, dataExportUID,
			dev1alpha1.AnnotationUserPVCUIDKey, userPVCUID,
			ErrPVConflict)
	}

	if dataExportUID != "" && expect.DataExportUID != "" && dataExportUID != string(expect.DataExportUID) {
		return nil, fmt.Errorf("PV %s was taken over by another DataExport (%s=%s): %w",
			pv.Name, dev1alpha1.AnnotationDataExportUIDKey, dataExportUID, ErrPVConflict)
	}

	if userPVCUID != "" && expect.SourcePVCUID != "" && userPVCUID != string(expect.SourcePVCUID) {
		return nil, fmt.Errorf("PV %s records a different source claim (%s=%s): %w",
			pv.Name, dev1alpha1.AnnotationUserPVCUIDKey, userPVCUID, ErrPVConflict)
	}

	return &pvRecoveryInfo{
		UserPVCNamespace:      userPVCNamespace,
		UserPVCName:           userPVCName,
		UserPVCUID:            userPVCUID,
		DataExportNamespace:   dataExportNamespace,
		DataExportName:        dataExportName,
		DataExportUID:         dataExportUID,
		TargetKindShort:       targetKindShort,
		HashSuffix:            hashSuffix,
		OriginalReclaimPolicy: originalReclaimPolicy,
	}, nil
}

// handleInconsistentPV labels a PV as inconsistent when its data-export annotations
// are corrupted or it is in an unexpected state. The severity label helps administrators
// distinguish between cases:
//   - "warning": ClaimRef is nil or points to a PVC outside the controller namespace
//   - "error": ClaimRef points to a PVC in the controller namespace (likely our export PVC)
func (r *DataexportReconciler) handleInconsistentPV(ctx context.Context, pv *corev1.PersistentVolume) error {
	severity := SeverityWarning
	if pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Namespace == r.Config.ControllerNamespace {
		severity = SeverityError
	}

	updatedPV := pv.DeepCopy()
	if len(updatedPV.Labels) == 0 {
		updatedPV.Labels = make(map[string]string)
	}
	updatedPV.Labels[dev1alpha1.LabelPVDataExporterInconsistent] = severity

	log.Printf("Labeling PV %s as inconsistent with severity %q", pv.Name, severity)
	if err := r.Client.Patch(ctx, updatedPV, client.MergeFromWithOptions(pv, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to label PV %s as inconsistent: %w", pv.Name, err)
	}

	return nil
}

// ensureUserPVCExportingAnnotationAndFinalizer marks the user's claim as being exported and returns it,
// so callers can record its identity (a name alone does not survive a delete/recreate).
func (r *DataexportReconciler) ensureUserPVCExportingAnnotationAndFinalizer(ctx context.Context, namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	userPVC := &corev1.PersistentVolumeClaim{}

	userPVCNamespacedName := types.NamespacedName{Namespace: namespace, Name: name}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		err := r.Client.Get(ctx, userPVCNamespacedName, userPVC)
		if err != nil {
			return false, err
		}

		// set annotation and finalizer
		hasAnnotation := userPVC.Annotations[DataExportInProgressKey] == "true"
		if !hasAnnotation {
			if userPVC.Annotations == nil {
				userPVC.Annotations = map[string]string{}
			}
			userPVC.Annotations[DataExportInProgressKey] = "true"
		}
		hasFinalizer := ContainsString(userPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		if !hasFinalizer {
			userPVC.Finalizers = append(userPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		}

		if (!hasAnnotation) || (!hasFinalizer) {
			err = r.Client.Update(ctx, userPVC)
			if err != nil {
				// continue attempts
				return false, nil
			}
			return true, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return userPVC, nil
}

// restoreOriginalPVState restores PV to its original state after export is complete:
// 1) Restores original reclaimPolicy from annotation (if saved, else return error)
// 2) Removes all storage manager annotations and labels
func (r *DataexportReconciler) restoreOriginalPVState(ctx context.Context, pv *corev1.PersistentVolume) error {
	updatedPV := pv.DeepCopy()

	needUpdate, err := restorePVReclaimPolicy(updatedPV)

	if err != nil {
		return err
	}

	if removePVExportMetadata(updatedPV) {
		needUpdate = true
	}

	if !needUpdate {
		log.Printf("PV %s already in original state, nothing to restore", pv.Name)
		return nil
	}

	log.Printf("Restoring PV %s to original state", pv.Name)
	if err := r.Client.Patch(ctx, updatedPV, client.MergeFromWithOptions(pv, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to restore PV %s: %w", pv.Name, err)
	}

	log.Printf("Successfully restored PV %s to original state", pv.Name)
	return nil
}

// restorePVReclaimPolicy restores PV's reclaimPolicy from annotation.
// Returns true if PV was modified. Returns error if annotation is missing or invalid.
func restorePVReclaimPolicy(pv *corev1.PersistentVolume) (bool, error) {
	originalPolicy, exists := pv.Annotations[dev1alpha1.AnnotationOriginalReclaimPolicyKey]

	if !exists || originalPolicy == "" {
		return false, fmt.Errorf("PV %s does not have %s annotation with original reclaim policy", pv.Name, dev1alpha1.AnnotationOriginalReclaimPolicyKey)
	}

	// Validate the policy value
	switch corev1.PersistentVolumeReclaimPolicy(originalPolicy) {
	case corev1.PersistentVolumeReclaimRetain,
		corev1.PersistentVolumeReclaimDelete,
		corev1.PersistentVolumeReclaimRecycle:
		// Valid policy
	default:
		return false, fmt.Errorf("invalid original reclaimPolicy %q for PV %s", originalPolicy, pv.Name)
	}

	if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimPolicy(originalPolicy) {
		return false, nil
	}

	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimPolicy(originalPolicy)
	log.Printf("Restoring reclaimPolicy %s for PV %s", originalPolicy, pv.Name)

	return true, nil
}

func (r *DataexportReconciler) removeUserPVCExportingAnnotationsAndFinalizer(ctx context.Context, userPVC *corev1.PersistentVolumeClaim) error {
	if userPVC == nil {
		return fmt.Errorf("nil pointer for user PVC")
	}

	annotationsToRemove := []string{
		DataExportInProgressKey,
		DataExportRequestAnnotationKey,
	}

	userPVCNamespacedName := types.NamespacedName{Namespace: userPVC.Namespace, Name: userPVC.Name}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		err := r.Client.Get(ctx, userPVCNamespacedName, userPVC)
		if err != nil {
			return false, err
		}

		// remove annotation and finalizer
		hasAnnotation := false
		if userPVC.Annotations != nil {
			for _, annotation := range annotationsToRemove {
				if _, exists := userPVC.Annotations[annotation]; exists {
					hasAnnotation = true
					delete(userPVC.Annotations, annotation)
				}
			}
		}

		hasFinalizer := ContainsString(userPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		if hasFinalizer {
			userPVC.Finalizers = RemoveString(userPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		}

		if (hasAnnotation) || (hasFinalizer) {
			err = r.Client.Update(ctx, userPVC)
			if err != nil {
				// continue attempts
				return false, nil
			}
			return true, nil
		}
		return true, nil
	})
	if err != nil {
		return err
	}

	return nil
}

// getExportPVC returns the export claim, or (nil, nil) when it does not exist. It is the plain existence
// read shared by provisioning and by the drift/loss classification of a serving export.
func (r *DataexportReconciler) getExportPVC(ctx context.Context, exportPVCName string) (*corev1.PersistentVolumeClaim, error) {
	exportPVC := &corev1.PersistentVolumeClaim{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: exportPVCName}, exportPVC)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get exportPVC from cache: %w", err)
	}
	return exportPVC, nil
}

// Check for exportPVC exist and doesn't has status Lost (so has Pending or Bound)
func (r *DataexportReconciler) validateExportPVC(ctx context.Context, dataExport *dev1alpha1.DataExport, exportPVCName string) (*corev1.PersistentVolumeClaim, error) {
	exportPVC, err := r.getExportPVC(ctx, exportPVCName)
	if err != nil || exportPVC == nil {
		return nil, err
	}
	if exportPVC.Status.Phase == corev1.ClaimLost {
		return nil, fmt.Errorf("export PVC for dataExport %s already exists and has status Lost", dataExport.GetName())
	}
	return exportPVC, nil
}

func (r *DataexportReconciler) validateExportDeploy(ctx context.Context, dataExport *dev1alpha1.DataExport, exportDeployName string) (*appsv1.Deployment, error) {
	exportDeploy := &appsv1.Deployment{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: exportDeployName}, exportDeploy)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get export deployment from cache: %w", err)
	}

	err = r.waitingForDeploymentLaunch(ctx, dataExport, exportDeployName)
	if err != nil {
		return nil, err
	}
	return exportDeploy, nil
}

// clearDataExportProviding undoes everything this export set up: it works out what was borrowed and hands
// the job to the one primitive that knows how to give it back safely.
//
// It does not touch the finalizer. Whether the object may now be deleted, settled or left alone is the
// caller's decision, and it may only be taken once this returns done.
func (r *DataexportReconciler) clearDataExportProviding(
	ctx context.Context,
	dataExport *dev1alpha1.DataExport,
	generatedNames Names,
) (bool, *recoveryBarrier, error) {
	log.Printf("Start recovering configuration before Dataexport %s", dataExport.GetName())

	takeover, err := r.resolveTakeover(ctx, dataExport, generatedNames)
	if err != nil {
		return false, nil, err
	}
	return r.reconcileLiveExportRecovery(ctx, generatedNames, takeover)
}

var ErrPVCValidationFailed = errors.New("PVC validation failed")

// Validate PV for attach to DataExport pod:
// - PVC in working state: has status "Bound" and PV has references to PVC
// - PVC detached from consumering pods: no pods, using PVC, no volumeAttachment with status "True"
func (r *DataexportReconciler) validateUserPVCAndGetPV(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolume, error) {
	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return nil, fmt.Errorf("%w: user's PVC %s does not contain VolumeName", ErrPVCValidationFailed, pvc.GetName())
	}

	pv := &corev1.PersistentVolume{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		return nil, fmt.Errorf("failed to get PV %s: %w", pvName, err)
	}

	// Check condition: PVC has status "Bound" and PV has references to PVC
	if err := validateUserPVCInWorkingState(pvc, pv); err != nil {
		return nil, err
	}

	// Check condition: no pods, using PVC
	// All pods in namespace
	podList := &corev1.PodList{}
	if err := r.Reader.List(ctx, podList, client.InNamespace(pvc.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to get podList: %w", err)
	}

	// Check for pods, using required PVC
	targetPVC := pvc.GetName()
	var foundPods []corev1.Pod
	for _, pod := range podList.Items {
		for _, volume := range pod.Spec.Volumes {
			if (volume.PersistentVolumeClaim != nil) && (volume.PersistentVolumeClaim.ClaimName == targetPVC) {
				foundPods = append(foundPods, pod)
			}
		}
	}
	if len(foundPods) > 0 {
		foundPodsNames := make([]string, 0, len(foundPods))
		for _, pod := range foundPods {
			foundPodsNames = append(foundPodsNames, pod.GetName())
		}
		return nil, fmt.Errorf("%w: user's PVC isn't free because it's being occupied by pods %s", ErrPVCValidationFailed, strings.Join(foundPodsNames, ", "))
	}

	// Check condition: no volumeAttachment with status "True"
	// Get volumeAttachmentList
	vaList := &storagev1.VolumeAttachmentList{}
	if err := r.Client.List(ctx, vaList, client.InNamespace(pvc.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to get volumeAttachmentList: %w", err)
	}

	// Find volumeAttachment for target PV
	targetPV := pv.GetName()
	var foundVAs []storagev1.VolumeAttachment
	for _, va := range vaList.Items {
		if (va.Spec.Source.PersistentVolumeName != nil) && (*va.Spec.Source.PersistentVolumeName == targetPV) && (va.Status.Attached) {
			foundVAs = append(foundVAs, va)
		}
	}
	if len(foundVAs) > 0 {
		foundVANames := make([]string, 0, len(foundVAs))
		for _, foundVA := range foundVAs {
			foundVANames = append(foundVANames, foundVA.GetName())
		}

		return nil, fmt.Errorf("%w: user's PV not free because has volumeAttachments with status True: %s", ErrPVCValidationFailed, strings.Join(foundVANames, ", "))
	}

	return pv, nil
}

// Validate user's PVC+PV for attach to DataExport pod:
// - PVC has status "Bound"
// - PV has references to PVC
func validateUserPVCInWorkingState(pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) error {
	// Check condition: PVC has status "Bound"
	if pvc.Status.Phase != corev1.ClaimBound {
		return fmt.Errorf("%w: user's PVC has status: %s, but expected status Bound", ErrPVCValidationFailed, pvc.Status.Phase)
	}

	// Check: PVC.Spec.VolumeName matches PV.Name
	if pvc.Spec.VolumeName == "" || pvc.Spec.VolumeName != pv.Name {
		return fmt.Errorf("%w: PVC volumeName %s does not match PV name %s",
			ErrPVCValidationFailed, pvc.Spec.VolumeName, pv.Name)
	}

	// Check condition: PV has references to PVC
	if pv.Spec.ClaimRef == nil {
		return fmt.Errorf("%w: user's PV does not contain ClaimRef", ErrPVCValidationFailed)
	}

	// Check ClaimRef: Name + Namespace must match
	ref := pv.Spec.ClaimRef
	if ref.Name != pvc.Name || ref.Namespace != pvc.Namespace {
		return fmt.Errorf("%w: PV ClaimRef does not match user's PVC (pvc=%s/%s, claimRef=%s/%s)",
			ErrPVCValidationFailed,
			pvc.Namespace, pvc.Name,
			ref.Namespace, ref.Name)
	}

	// Check UID if exists
	if pvc.UID != "" && ref.UID != "" && pvc.UID != ref.UID {
		return fmt.Errorf("%w: PV ClaimRef UID mismatch (pvc uid=%s, claimRef uid=%s)",
			ErrPVCValidationFailed, pvc.UID, ref.UID)
	}

	return nil
}

// TODO: remove copy-paste, reuse common/deployment.go
func (r *DataexportReconciler) createDeployment(ctx context.Context, dataExport *dev1alpha1.DataExport, exportPVC *corev1.PersistentVolumeClaim, generatedNames Names) error {
	// Get dataExport image name from configMap
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: CongigMapName}, cm)
	if err != nil {
		return fmt.Errorf("failed to get ComfigMap from cache: %w", err)
	}
	dataExportPodImageName := cm.Data["image"]

	// Create deployment
	exportDeploy := appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      generatedNames.DeployName,
			Namespace: r.Config.ControllerNamespace,
			Labels: map[string]string{
				dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue,
			},
			// Carry the owning DataExport identity on the Deployment object itself (not just the pod
			// template) so the controller's Deployment watch maps a mid-life delete/change back to this
			// DataExport (createRequest reads these annotations) and drift-repair fires promptly.
			Annotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: dataExport.Namespace,
				dev1alpha1.AnnotationStorageManagerNameKey:      dataExport.Name,
			},
		},
		Spec: makeExportDeploySpec(exportPVC.Spec.VolumeMode, dataExport, dataExportPodImageName, dataExport.Spec.Ttl, r.Config.ControllerNamespace, generatedNames),
	}

	err = r.Client.Create(ctx, &exportDeploy)
	if err != nil {
		return fmt.Errorf("failed to create DataExport deployment: %w", err)
	}
	log.Printf("DataExport deployment created!")

	err = r.waitingForDeploymentLaunch(ctx, dataExport, generatedNames.DeployName)
	if err != nil {
		return err
	}

	return nil
}

func (r *DataexportReconciler) waitingForDeploymentLaunch(ctx context.Context, dataExport *dev1alpha1.DataExport, deployName string) error {
	// waiting for deployment launch
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		deploy := &appsv1.Deployment{}

		err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: deployName}, deploy)
		if err != nil {
			if kubeerrors.IsNotFound(err) {
				// Deployment not created yet, continie waiting
				return false, nil
			}
			return false, err
		}

		// Check deployment conditions
		for _, condition := range deploy.Status.Conditions {
			if condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse {
				return false, fmt.Errorf("deployment %q is stuck: %s: %w", deployName, condition.Message, ErrDeploymentFailed)
			}
			if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
				return false, fmt.Errorf("deployment %q has replica failure: %s: %w", deployName, condition.Message, ErrDeploymentFailed)
			}
		}

		// Check for all replicas started
		if deploy.Status.AvailableReplicas == *deploy.Spec.Replicas {
			// The deployment is up; wait for the exporter pod to report it is actually serving
			// (serverState=Ready, written by the pod once it is listening and its CA is published) before
			// flipping Ready=True/ServerReady. Re-read the DataExport so the serverState the pod just wrote
			// is observed inside this poll.
			fresh := &dev1alpha1.DataExport{}
			if getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: dataExport.Namespace, Name: dataExport.Name}, fresh); getErr != nil {
				return false, getErr
			}
			if ServerState(fresh.Status.ServerState) != ServerStateReady {
				log.Printf("Deployment %q available; awaiting exporter serverState=Ready\n", deployName)
				return false, nil
			}
			readyCond := meta.FindStatusCondition(dataExport.Status.Conditions, string(ConditionReady))
			if readyCond == nil || readyCond.Reason != string(ReasonExpired) {
				meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
					Type:               string(ConditionReady),
					Status:             metav1.ConditionTrue,
					Reason:             string(ReasonServerReady),
					Message:            "Server is ready and export started",
					ObservedGeneration: dataExport.Generation,
				})
			}
			return true, nil
		}

		log.Printf("Waiting for deployment %q: %d/%d replicas available\n",
			deployName,
			deploy.Status.AvailableReplicas,
			*deploy.Spec.Replicas,
		)
		return false, nil
	})
	if err != nil && !errors.Is(err, ErrDeploymentFailed) {
		return fmt.Errorf("timed out waiting for deployment %q to become ready: %w: %w", deployName, ErrDeploymentFailed, err)
	}
	return err
}

func makeExportDeploySpec(pvMode *corev1.PersistentVolumeMode, dataExport *dev1alpha1.DataExport, dataExportPodImageName, ttl, controllerNamespace string, generatedNames Names) appsv1.DeploymentSpec {
	deploySpec := appsv1.DeploymentSpec{
		Replicas: Int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				dev1alpha1.LabelApplicationKey:                  dev1alpha1.LabelDataExportValue,
				dev1alpha1.LabelStorageManagerDeploymentNameKey: generatedNames.DeployName,
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					dev1alpha1.LabelApplicationKey:                  dev1alpha1.LabelDataExportValue,
					dev1alpha1.LabelStorageManagerDeploymentNameKey: generatedNames.DeployName,
				},
				Annotations: map[string]string{
					dev1alpha1.AnnotationStorageManagerNamespaceKey: dataExport.Namespace,
					dev1alpha1.AnnotationStorageManagerNameKey:      dataExport.Name,
				},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: ServiceAccountServer,
				ImagePullSecrets:   []corev1.LocalObjectReference{{Name: ImagePullSecretsName}},
				Containers:         makeContainerList(pvMode, dataExportPodImageName, ttl, dataExport.Namespace, dataExport.Name, controllerNamespace, generatedNames),
				Volumes:            makeVolumeList(generatedNames.ExportPVCName),
			},
		},
	}
	return deploySpec
}

func makeContainerList(pvMode *corev1.PersistentVolumeMode, image, ttl, dataExportNamespace, dataExportName, controllerNamespace string, generatedNames Names) []corev1.Container {
	var containers []corev1.Container
	portArg := fmt.Sprintf("--port=%d", FileServerPort)
	ttlArg := fmt.Sprintf("--ttl=%s", ttl)
	dataExportNamespaceArg := fmt.Sprintf("--data-export-namespace=%s", dataExportNamespace)
	dataExportNameArg := fmt.Sprintf("--data-export-name=%s", dataExportName)
	exportTargetKindShortArg := fmt.Sprintf("--export-target-kind-short=%s", generatedNames.TargetKindShort)
	exportTargetNameArg := fmt.Sprintf("--export-target-name=%s", generatedNames.TargetName)
	dataExportCASecretNameArg := fmt.Sprintf("--data-export-ca-secret-name=%s", generatedNames.CASecretName)
	dataExportServiceNameArg := fmt.Sprintf("--data-export-service-name=%s", generatedNames.HeadlessServiceName)
	controllerNamespaceArg := fmt.Sprintf("--controller-namespace=%s", controllerNamespace)

	switch *pvMode {
	case corev1.PersistentVolumeBlock:
		var rootUser int64
		securityContext := corev1.SecurityContext{RunAsUser: &rootUser}
		containers = []corev1.Container{
			{
				Name:  "data-exporter",
				Image: image,
				Args:  []string{"--mode=block", "--path=/mnt/block-storage", portArg, ttlArg, dataExportNamespaceArg, dataExportNameArg, exportTargetKindShortArg, exportTargetNameArg, dataExportCASecretNameArg, dataExportServiceNameArg, controllerNamespaceArg},
				Env: []corev1.EnvVar{
					{
						Name: "POD_IP",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "status.podIP",
							},
						},
					},
				},
				VolumeDevices: []corev1.VolumeDevice{
					{
						Name:       "export-pvc",
						DevicePath: "/mnt/block-storage",
					},
				},
				Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: FileServerPort}},
				SecurityContext: &securityContext,
			},
		}

	case corev1.PersistentVolumeFilesystem:
		var rootUser int64
		securityContext := corev1.SecurityContext{RunAsUser: &rootUser}
		containers = []corev1.Container{
			{
				Name:  "data-exporter",
				Image: image,
				Args:  []string{"--mode=filesystem", "--path=/mnt/filesystem-storage", portArg, ttlArg, dataExportNamespaceArg, dataExportNameArg, exportTargetKindShortArg, exportTargetNameArg, dataExportCASecretNameArg, dataExportServiceNameArg, controllerNamespaceArg},
				Env: []corev1.EnvVar{
					{
						Name: "POD_IP",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "status.podIP",
							},
						},
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "export-pvc",
						MountPath: "/mnt/filesystem-storage",
					},
				},
				Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: FileServerPort}},
				SecurityContext: &securityContext,
			},
		}
	}
	return containers
}

func makeVolumeList(pvcName string) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "export-pvc",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
					ReadOnly:  true,
				},
			},
		},
	}
	return volumes
}

func (r *DataexportReconciler) getExportPVCFromUserPVC(ctx context.Context, userPVCNameSpace, userPVCName, exportPVCName string, dataExport *dev1alpha1.DataExport) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, error) {
	// Get user PVC // TODO: check the PV name in Lost state
	userPVC := &corev1.PersistentVolumeClaim{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: userPVCNameSpace, Name: userPVCName}, userPVC)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("user PVC %s/%s: %w: %w", userPVCNameSpace, userPVCName, ErrTargetNotFound, err)
		}
		return nil, nil, err
	}

	pv, err := r.validateUserPVCAndGetPV(ctx, userPVC)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to validate user PVC: %w", err)
	}

	if pv == nil {
		return nil, nil, fmt.Errorf("user PVC %s does not have a valid PersistentVolume", userPVC.GetName())
	}

	// Create export PVC
	exportPVC, err := r.detachPVC(ctx, userPVC, pv, exportPVCName, dataExport)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to detach user PVC: %w", err)
	}

	return exportPVC, pv, nil
}

func (r *DataexportReconciler) getExportPVCFromUserVirtualDisk(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames Names) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, string, error) {
	userNamespace := dataExport.Namespace
	userVirtualDiskName := dataExport.Spec.TargetRef.Name

	log.Printf("Processing user VirtualDisk %s/%s export for DataExport resource %s", userNamespace, userVirtualDiskName, dataExport.Name)

	userPVCName, err := r.prepareVirtualDiskForExportAndGetPVCName(ctx, userNamespace, userVirtualDiskName)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to prepare user VirtualDisk %s/%s for export: %w", userNamespace, userVirtualDiskName, err)
	}

	userVirtualDiskReadyForExport, err := r.isVirtualDiskReadyForExport(ctx, userNamespace, userVirtualDiskName)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to check if VirtualDisk %s/%s is ready for export: %w", userNamespace, userVirtualDiskName, err)
	}

	if !userVirtualDiskReadyForExport {
		return nil, nil, "", fmt.Errorf("%w: VirtualDisk %s/%s is not ready for export", ErrTargetNotReady, userNamespace, userVirtualDiskName)
	}

	log.Printf("User VirtualDisk %s/%s is ready, using PVC %s/%s for export", userNamespace, userVirtualDiskName, userNamespace, userPVCName)

	exportPVC, pv, err := r.getExportPVCFromUserPVC(ctx, userNamespace, userPVCName, generatedNames.ExportPVCName, dataExport)
	return exportPVC, pv, userPVCName, err
}

func (r *DataexportReconciler) prepareVirtualDiskForExportAndGetPVCName(ctx context.Context, namespace, virtualDiskName string) (string, error) {
	log.Printf("Preparing user VirtualDisk %s/%s for export", namespace, virtualDiskName)

	// Fetch the VirtualDisk resource
	virtualDisk := &virtv1alpha2.VirtualDisk{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: virtualDiskName}, virtualDisk)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return "", fmt.Errorf("VirtualDisk %s/%s: %w: %w", namespace, virtualDiskName, ErrTargetNotFound, err)
		}
		return "", fmt.Errorf("failed to get VirtualDisk %s/%s: %w", namespace, virtualDiskName, err)
	}

	// Check if the VirtualDisk has a valid PersistentVolumeClaim in status
	pvcName := virtualDisk.Status.Target.PersistentVolumeClaim
	if pvcName == "" {
		return "", fmt.Errorf("user VirtualDisk %s/%s does not have a valid PersistentVolumeClaim in status: %w", namespace, virtualDiskName, ErrTargetNotReady)
	}

	// Fetch the PVC to ensure it exists and is in a valid state
	pvc := &corev1.PersistentVolumeClaim{}
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pvcName}, pvc)
	if err != nil {
		return "", fmt.Errorf("failed to get PVC %s/%s for user VirtualDisk %s/%s: %w", namespace, pvcName, namespace, virtualDiskName, err)
	}

	// Ensure the PVC has the DataExportRequest annotation
	err = r.ensureAnnotationsOnPVC(ctx, pvc, map[string]string{
		DataExportRequestAnnotationKey: "true",
	})
	if err != nil {
		return "", fmt.Errorf("failed to ensure DataExportRequest annotation %s on PVC %s/%s: %w", DataExportRequestAnnotationKey, namespace, pvcName, err)
	}

	return pvcName, nil
}

// Check if the VirtualDisk is already in use for export

func (r *DataexportReconciler) isVirtualDiskReadyForExport(ctx context.Context, namespace, virtualDiskName string) (bool, error) {
	log.Printf("Checking if user VirtualDisk %s/%s is ready for export", namespace, virtualDiskName)

	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		// Re-fetch the VirtualDisk to ensure we have the latest state
		virtualDisk := &virtv1alpha2.VirtualDisk{}
		err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: virtualDiskName}, virtualDisk)
		if err != nil {
			return false, fmt.Errorf("failed to get user VirtualDisk %s/%s while checking readiness: %w", namespace, virtualDiskName, err)
		}
		if virtualDisk.Status.Conditions == nil {
			return false, nil // Conditions not set yet, keep waiting
		}

		isReady := false
		isReadyForExport := false

		for _, condition := range virtualDisk.Status.Conditions {
			if condition.Type == vdcondition.ReadyType.String() {
				isReady = isVDReady(virtualDisk, condition)
			}
			if condition.Type == vdcondition.InUseType.String() {
				if condition.Status == metav1.ConditionTrue &&
					condition.Reason == "UsedForDataExport" && // TODO: get rid of this hardcoded reason
					virtualDisk.Generation == condition.ObservedGeneration {
					isReadyForExport = true
				}
			}
		}

		if isReady && isReadyForExport {
			log.Printf("VirtualDisk %s/%s is ready for export", namespace, virtualDiskName)
			return true, nil
		}

		log.Printf("VirtualDisk %s/%s is not ready for export yet, waiting...", namespace, virtualDiskName)
		return false, nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to check if user VirtualDisk %s/%s is ready for export: %w", namespace, virtualDiskName, err)
	}

	log.Printf("User VirtualDisk %s/%s is ready for export", namespace, virtualDiskName)
	return true, nil
}

func (r *DataexportReconciler) ensureAnnotationsOnPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim, annotationsToAdd map[string]string) error {
	if pvc == nil {
		return fmt.Errorf("PVC is nil, cannot ensure annotations")
	}

	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}

	needUpdate := false

	for key, value := range annotationsToAdd {
		if pvc.Annotations[key] != value {
			pvc.Annotations[key] = value
			needUpdate = true
		}
	}

	if needUpdate {
		err := r.Client.Update(ctx, pvc)
		if err != nil {
			return fmt.Errorf("failed to update PVC %s/%s with annotation %+v: %w", pvc.Namespace, pvc.Name, annotationsToAdd, err)
		}
	}

	return nil
}

func isVDReady(vd *virtv1alpha2.VirtualDisk, condition metav1.Condition) bool {
	if vd.Generation != condition.ObservedGeneration {
		return false
	}

	switch condition.Status {
	case metav1.ConditionTrue:
		return true
	case metav1.ConditionFalse:
		// VD is ready for us if condition Reason is "Exporting"
		return condition.Reason == "Exporting" // TODO: get rid of this hardcoded reason
	default:
		return false
	}
}

// validateDataExportSpec performs cheap, permanent-until-spec-change validation. With the GroupKind
// targetRef (C6) there is no kind allowlist: classifyTargetRef rejects structurally invalid / forbidden
// targets (e.g. a bare VolumeSnapshotContent), and only the live-VirtualDisk path needs a CRD presence
// pre-check (the snapshot path is generic and surfaces missing targets through Ready conditions).
// validateDataExportSpec performs cheap, permanent-until-spec-change validation. It returns an
// ErrTerminal-wrapped error for genuine spec/target problems (mapped to phase=Failed by the caller) and a
// plain (retryable) error for transient API failures — notably a failure to probe the VirtualDisk CRD,
// which must NOT be collapsed into "CRD does not exist" and fail a healthy export.
func (r *DataexportReconciler) validateDataExportSpec(ctx context.Context, dataExport *dev1alpha1.DataExport) error {
	cat, _, err := classifyTargetRef(dataExport.Spec.TargetRef.Group, dataExport.Spec.TargetRef.Kind)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrTerminal, err)
	}

	if cat == categoryLiveVirtualDisk {
		exists, err := r.isVirtualDiskCRDExists(ctx)
		if err != nil {
			// Transient API error probing the CRD — retryable, not a validation failure.
			return err
		}
		if !exists {
			return fmt.Errorf("%w: CRD %s does not exist in the cluster", ErrTerminal, virtv1alpha2.VirtualDiskKind)
		}
	}

	return nil
}

// isVirtualDiskCRDExists reports whether the VirtualDisk CRD is registered. A NotFound means it is
// genuinely absent (false, nil); any other error is transient and is propagated so the caller can retry
// instead of mistaking an API hiccup for an absent CRD.
func (r *DataexportReconciler) isVirtualDiskCRDExists(ctx context.Context) (bool, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := r.Reader.Get(ctx, types.NamespacedName{Name: dev1alpha1.VirtualDiskCRDName}, crd)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking for VirtualDisk CRD existence: %w", err)
	}
	return true, nil
}

// validatePVNotOwnedByAnotherDataExport checks if the PV is already being used
// by a different DataExport. This prevents conflicts when multiple DataExports
// attempt to export the same PVC simultaneously. Returns an error if the PV
// has storage manager annotations pointing to a different DataExport.
func validatePVNotOwnedByAnotherDataExport(pv *corev1.PersistentVolume, expectedNamespace, expectedName string) error {
	annotations := pv.Annotations

	if len(annotations) == 0 {
		return nil
	}

	currentName, hasName := annotations[dev1alpha1.AnnotationStorageManagerNameKey]
	currentNamespace, hasNS := annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey]

	if !hasNS && !hasName {
		return nil
	}

	if !hasNS || !hasName {
		return fmt.Errorf("PV %s has inconsistent storage manager annotations: %w", pv.Name, ErrPVConflict)
	}

	if currentName != expectedName || currentNamespace != expectedNamespace {
		return fmt.Errorf("PV %s is already owned by DataExport %s/%s: %w", pv.Name, currentNamespace, currentName, ErrPVConflict)
	}

	return nil
}

// removePVExportMetadata removes storage manager annotations and labels from PV.
// Returns true if any metadata was removed
func removePVExportMetadata(updatePV *corev1.PersistentVolume) bool {
	changed := false
	keysToRemove := []string{
		dev1alpha1.AnnotationUserPVCNamespaceKey,
		dev1alpha1.AnnotationUserPVCNameKey,
		dev1alpha1.AnnotationStorageManagerNamespaceKey,
		dev1alpha1.AnnotationStorageManagerNameKey,
		dev1alpha1.AnnotationPVTargetKindShortKey,
		dev1alpha1.AnnotationPVHashSuffixKey,
		dev1alpha1.AnnotationOriginalReclaimPolicyKey,
		// The takeover identity goes with the rest: a PV returned to its owner records no takeover, and a
		// leftover UID here would make the next export of the same volume look like someone else's.
		dev1alpha1.AnnotationDataExportUIDKey,
		dev1alpha1.AnnotationUserPVCUIDKey,
	}

	// Remove annotations
	for _, key := range keysToRemove {
		if _, exists := updatePV.Annotations[key]; exists {
			delete(updatePV.Annotations, key)
			changed = true
		}
	}

	// Remove labels
	if _, exists := updatePV.Labels[dev1alpha1.LabelPVDataExporter]; exists {
		delete(updatePV.Labels, dev1alpha1.LabelPVDataExporter)
		changed = true
	}

	return changed
}

// removeOrphanResources handles cleanup when a DataExport is deleted while the controller was down. It
// finds the orphaned PV by label, reads what it can from the annotations, and hands the teardown to the
// same primitive every other path uses — the parent being gone changes who asks, not what has to happen
// to the volume. Triggered by the PV watch when the DataExport is not found during reconciliation.
//
// There is no finalizer to manage here: the parent it would have protected is already gone.
func (r *DataexportReconciler) removeOrphanResources(ctx context.Context, dataExportNamespace, dataExportName string) (*recoveryBarrier, error) {
	log.Printf("Starting cleanup of orphaned resources for deleted DataExport %s/%s", dataExportNamespace, dataExportName)

	// List PVs with data-exporter label. The cache now contains all PVs (no label filter),
	// so we must use MatchingLabels to filter only PVs managed by DataExport.
	pvList := &corev1.PersistentVolumeList{}
	if err := r.Client.List(ctx, pvList, client.MatchingLabels{dev1alpha1.LabelPVDataExporter: "true"}); err != nil {
		return nil, fmt.Errorf("failed to list PVs for orphan cleanup: %w", err)
	}

	for i := range pvList.Items {
		pv := &pvList.Items[i]

		// skip, its not orphaned PV
		if pv.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] != dataExportName ||
			pv.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] != dataExportNamespace {
			continue
		}

		log.Printf("Found orphaned PV %s for deleted DataExport %s/%s", pv.Name, dataExportNamespace, dataExportName)

		// Validate and extract recovery info from PV annotations.
		// If annotations are corrupted, label the PV as inconsistent and stop reconcile
		// instead of attempting cleanup with wrong values.
		// The parent is gone, so no UID can be proven here: the expectation carries namespace/name only
		// and the UID checks are skipped.
		pvInfo, err := parsePVRecoveryInfo(pv, pvOwnerExpectation{
			DataExportNamespace: dataExportNamespace,
			DataExportName:      dataExportName,
		})
		if err != nil {
			log.Printf("PV %s has inconsistent state: %v", pv.Name, err)
			if labelErr := r.handleInconsistentPV(ctx, pv); labelErr != nil {
				return nil, fmt.Errorf("failed to label inconsistent PV %s: %w", pv.Name, labelErr)
			}
			return nil, nil // Stop reconcile without error
		}

		// The names come from the suffix recorded when the resources were created, not from deriving one
		// again: the sweep must find the infrastructure that exists, not the infrastructure a
		// recomputation says should exist. The claim name is only ever the claim to give the volume back
		// to; no generated name depends on it.
		names := NewNamesFromPersistedIdentity(pvInfo.TargetKindShort, pvInfo.HashSuffix)
		takeover := takeoverRef{
			PVName:       pv.Name,
			SourcePVCUID: pv.Annotations[dev1alpha1.AnnotationUserPVCUIDKey],
		}
		if pvInfo.UserPVCName != "" {
			// Snapshot-backed exports detach nobody, so they leave these empty and only the volume's own
			// metadata has to be cleaned up.
			takeover.SourceClaim = types.NamespacedName{Namespace: pvInfo.UserPVCNamespace, Name: pvInfo.UserPVCName}
		}

		_, blocked, err := r.reconcileLiveExportRecovery(ctx, names, takeover)
		if err != nil {
			return nil, fmt.Errorf("failed to clean up after deleted DataExport %s/%s: %w", dataExportNamespace, dataExportName, err)
		}
		if blocked != nil {
			return blocked, nil
		}

		// One DataExport operates on exactly one PV, so we stop after finding the matching one
		break
	}

	log.Printf("cleanup complete: %s/%s", dataExportNamespace, dataExportName)
	return nil, nil
}
