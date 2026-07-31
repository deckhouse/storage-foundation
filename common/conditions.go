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

package common

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// type conditionField string
type ConditionType string
type ConditionReason string

// Phase is the coarse-grained lifecycle state of a DataImport/DataExport, written EXCLUSIVELY by the
// data-manager controller (mirrors the virtualization VMOP status model). It is surfaced on
// status.phase and drives the printer STATUS column and the garbage collector's terminal-only filter.
type Phase string

const (
	// PhasePending — the controller has not yet driven the object to a serving state (endpoint not ready).
	// A permanently-pending object is legal (like VMOP) and is never garbage-collected or auto-failed.
	PhasePending Phase = "Pending"
	// PhaseReady — the server endpoint is up and serving (upload/download may proceed).
	PhaseReady Phase = "Ready"
	// PhaseCompleted — DataImport only: the artifact was produced successfully. Terminal.
	PhaseCompleted Phase = "Completed"
	// PhaseExpired — the idle-TTL window elapsed without activity. Terminal; a normal outcome, not a failure.
	PhaseExpired Phase = "Expired"
	// PhaseFailed — a terminal error prevented completion. Terminal.
	PhaseFailed Phase = "Failed"
	// PhaseTerminating — DeletionTimestamp != nil; the object is being cleaned up. Transient, not an outcome.
	PhaseTerminating Phase = "Terminating"
)

// IsTerminal reports whether a phase is a terminal outcome (Completed | Expired | Failed). The garbage
// collector only ever considers terminal, non-Terminating objects for deletion.
func (p Phase) IsTerminal() bool {
	switch p {
	case PhaseCompleted, PhaseExpired, PhaseFailed:
		return true
	default:
		return false
	}
}

// ServerState is the raw progress signal reported by the exporter/importer server pod on
// status.serverState. The pod is the ONLY writer; the controller derives phase and conditions from it.
type ServerState string

const (
	// ServerStateReady — the server started and is serving requests.
	ServerStateReady ServerState = "Ready"
	// ServerStateFinished — DataImport only: the client's upload finished and the server persisted the
	// fact durably before answering the client 200. The controller turns this into UploadFinished=True and
	// starts producing the durable artifact.
	ServerStateFinished ServerState = "Finished"
	// ServerStateIdleExpired — the idle-TTL window elapsed without activity (no in-flight transfer). The
	// controller turns this into the terminal Expired phase.
	ServerStateIdleExpired ServerState = "IdleExpired"
)

const (
	ConditionReady          ConditionType = "Ready"
	ConditionUploadFinished ConditionType = "UploadFinished"
	ConditionCompleted      ConditionType = "Completed"

	// Progress / terminal reasons.
	ReasonPending        ConditionReason = "Pending"
	ReasonPVCCreated     ConditionReason = "PVCCreated"
	ReasonServerReady    ConditionReason = "ServerReady"
	ReasonInProgress     ConditionReason = "InProgress"
	ReasonExpired        ConditionReason = "Expired"
	ReasonDeleted        ConditionReason = "Deleted"
	ReasonUploadFinished ConditionReason = "UploadFinished"
	ReasonCompleted      ConditionReason = "Completed"
	ReasonFailed         ConditionReason = "Failed"

	// Not ready error reasons (shared by DataExport and DataImport).
	ReasonPublishFailed    ConditionReason = "PublishFailed"
	ReasonValidationFailed ConditionReason = "ValidationFailed"
	ReasonTargetNotFound   ConditionReason = "TargetNotFound"
	ReasonTargetNotReady   ConditionReason = "TargetNotReady"
	ReasonTargetFailed     ConditionReason = "TargetFailed"
	ReasonPVConflict       ConditionReason = "PVConflict"
	ReasonDeploymentFailed ConditionReason = "DeploymentFailed"
	ReasonCleanupFailed    ConditionReason = "CleanupFailed"

	// Managed-resource drift reasons. The condition reason stays coarse (which class of drift), while
	// the message carries the specifics (role, kind, name, expected/actual UID) and status.cleanupReason
	// carries the machine-readable discriminator that selects the recovery path.
	//
	// ReasonManagedResourceLost — a resource the controller created and still depends on is gone.
	ReasonManagedResourceLost ConditionReason = "ManagedResourceLost"
	// ReasonManagedResourceIdentityMismatch — a resource with the expected name exists, but its UID is
	// not the one we created; it must not be adopted, mutated or deleted.
	ReasonManagedResourceIdentityMismatch ConditionReason = "ManagedResourceIdentityMismatch"
	// ReasonCleanupBlocked — recovery is running but a safety barrier has not been passed yet (a Pod
	// still consumes the volume, a VolumeAttachment survives, the binding has not converged). The
	// message names the blocking object. Barriers are never skipped on a timeout.
	ReasonCleanupBlocked ConditionReason = "CleanupBlocked"
)

// IsManagedResourceFailureReason reports whether a Ready reason denotes a managed-resource failure that
// ends the operation. Both drift reasons qualify: a lost child and a replaced child are equally
// unrecoverable for the current run, and the mismatch path must be able to reach a terminal phase after
// its recovery too. CleanupBlocked deliberately does not qualify — it is the transient "a barrier has not
// been passed yet" state, and treating it as terminal would abandon a recovery that is still progressing.
func IsManagedResourceFailureReason(reason ConditionReason) bool {
	switch reason {
	case ReasonManagedResourceLost, ReasonManagedResourceIdentityMismatch:
		return true
	default:
		return false
	}
}

// CleanupReason is the internal discriminator persisted in status.cleanupReason. A non-empty value
// means the object must run failure-driven recovery before it may become terminal, and selects which
// recovery path runs. Unlike a condition reason it is not primarily a user-facing string: it is the
// crash-safe checkpoint that survives a controller restart mid-recovery.
type CleanupReason string

const (
	// CleanupReasonExportPVCPostRebindLost — the export claim disappeared after the PV had already been
	// rebound to it, so the PV now points at a claim UID that no longer exists. Recreating a same-named
	// claim would not restore that binding; the volume must be returned to the user and the export failed.
	CleanupReasonExportPVCPostRebindLost CleanupReason = "ExportPVCPostRebindLost"
	// CleanupReasonExportPVCIdentityMismatch — a claim with the expected name exists but carries a
	// foreign UID. Recovery runs (the volume is still ours to return), but the foreign claim is left
	// untouched.
	CleanupReasonExportPVCIdentityMismatch CleanupReason = "ExportPVCIdentityMismatch"
)

// StripConditionsNotIn removes every status condition whose type is not in the allowed set and reports
// whether anything was removed. It migrates legacy objects onto the current condition catalog: existing
// DataImport/DataExport objects carry a stale condition (type "Expired") that the current controller
// seeded and the narrowed CRD condition-type enum no longer permits. status.conditions is an atomic list
// (no x-kubernetes-list-type), so a plain meta.SetStatusCondition leaves the stale entry in place and any
// subsequent Status().Update would be rejected with an enum-validation error (and validation ratcheting
// is absent on older Kubernetes). The controller MUST call this before its first status write so the
// persisted conditions list only ever contains the current catalog.
func StripConditionsNotIn(status *dev1alpha1.DataExportImportStatus, allowed ...ConditionType) bool {
	if status == nil || len(status.Conditions) == 0 {
		return false
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, t := range allowed {
		allowedSet[string(t)] = struct{}{}
	}
	kept := make([]metav1.Condition, 0, len(status.Conditions))
	changed := false
	for _, c := range status.Conditions {
		if _, ok := allowedSet[c.Type]; ok {
			kept = append(kept, c)
		} else {
			changed = true
		}
	}
	if changed {
		status.Conditions = kept
	}
	return changed
}

// SetCompletionTimestampOnce stamps status.completionTimestamp the first time the object reaches a
// terminal phase (Completed | Expired | Failed) and leaves it untouched afterwards, so the value records
// when the object first finished. The garbage collector measures retention age from this timestamp. It
// reports whether the timestamp was set (for change detection). now is injected for testability.
func SetCompletionTimestampOnce(status *dev1alpha1.DataExportImportStatus, phase Phase, now metav1.Time) bool {
	if status == nil {
		return false
	}
	if phase.IsTerminal() && status.CompletionTimestamp == nil {
		status.CompletionTimestamp = &now
		return true
	}
	return false
}

func GetCondition(conditions []metav1.Condition, conditionType ConditionType) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == string(conditionType) {
			return &conditions[i]
		}
	}

	return nil
}

// Sets a specific condition of the resource, creating it when absent and updating it in place when
// present. Returns true if the in-memory condition changed and the caller must persist the status.
//
// Delegates to meta.SetStatusCondition (the same helper the controllers use), which creates the
// condition when missing. The previous implementation only mutated an already-existing condition and
// silently no-op'd when it was absent, so conditions the controller never pre-seeds (e.g.
// UploadFinished) could never be set by the importer/exporter — leaving the import stuck because the
// populator waits for UploadFinished=True.
//
// The context and client parameters are unused; they are kept to preserve the call signature.
func UpdateCondition[T Object](
	_ context.Context,
	_ client.Client,
	resource T,
	conditionType ConditionType,
	status metav1.ConditionStatus,
	reason ConditionReason,
	message string,
) bool {
	return meta.SetStatusCondition(&resource.GetStatus().Conditions, metav1.Condition{
		Type:               string(conditionType),
		Status:             status,
		Reason:             string(reason),
		Message:            message,
		ObservedGeneration: resource.GetGeneration(),
	})
}
