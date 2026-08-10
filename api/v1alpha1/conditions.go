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

package v1alpha1

// Condition type constants
const (
	// ConditionTypeReady is set to True on success or False on final failure.
	ConditionTypeReady = "Ready"
	// ConditionTypeStalled reports that a request shows no observable progress. It is a
	// diagnostic axis independent of Ready and is NEVER terminal.
	//
	// Stall MUST NOT be reported by changing the reason of Ready: terminality is derived as
	// "Ready=False with a reason other than TargetsPending", so a stall reason placed on Ready
	// would make a live, still-executing request eligible for garbage collection.
	ConditionTypeStalled = "Stalled"
)

// Condition reason constants
const (
	// ConditionReasonCompleted indicates successful completion
	ConditionReasonCompleted = "Completed"
	// ConditionReasonInvalidMode indicates invalid mode was specified
	ConditionReasonInvalidMode = "InvalidMode"
	// ConditionReasonIncompatible indicates incompatible configuration (e.g., WFFC or cross-SC restore)
	ConditionReasonIncompatible = "Incompatible"
	// ConditionReasonInternalError indicates internal error
	ConditionReasonInternalError = "InternalError"
	// ConditionReasonNotFound indicates resource not found
	ConditionReasonNotFound = "NotFound"
	// ConditionReasonRBACDenied indicates RBAC permission denied
	ConditionReasonRBACDenied = "RBACDenied"
	// ConditionReasonInvalidSource indicates invalid source specified
	ConditionReasonInvalidSource = "InvalidSource"
	// ConditionReasonUnsupportedTargetKind indicates the restore target kind is not supported (only PersistentVolumeClaim for now)
	ConditionReasonUnsupportedTargetKind = "UnsupportedTargetKind"
	// ConditionReasonPVBound indicates PV is bound and cannot be detached
	ConditionReasonPVBound = "PVBound"
	// ConditionReasonSnapshotCreationFailed was previously set when the CSI VolumeSnapshotContent
	// reported status.error. That is no longer treated as terminal: the external-snapshotter sidecar
	// retries CreateSnapshot without a cap and clears status.error once ReadyToUse=true, and the error
	// carries no gRPC code to reliably classify terminal vs. transient. A CSI error now keeps the VCR
	// in the non-terminal TargetsPending state instead. The constant is retained for API stability
	// (exported, vendored by other repos) but is no longer emitted by the controller.
	ConditionReasonSnapshotCreationFailed = "SnapshotCreationFailed"
	// ConditionReasonTargetsPending indicates one or more capture targets are not ready yet
	ConditionReasonTargetsPending = "TargetsPending"
	// ConditionReasonRestoreFailed indicates restore operation failed
	ConditionReasonRestoreFailed = "RestoreFailed"
)

// Diagnostic reasons for the Stalled condition. All of them are non-terminal: they describe what
// is observable, never what is proven about the storage backend.
const (
	// ConditionReasonSnapshotStackUnavailable indicates the cluster-wide snapshot-controller has not
	// added its finalizer to the VolumeSnapshotContent, i.e. the snapshot stack itself is not running.
	ConditionReasonSnapshotStackUnavailable = "SnapshotStackUnavailable"
	// ConditionReasonSnapshotExecutionUnobservable indicates no executor has visibly picked the request
	// up: the snapshot-controller finalizer is present, but no result and no being-created annotation
	// appeared within the grace period. It deliberately does NOT claim that the snapshotter sidecar is
	// absent — that cannot be proven from observation alone.
	ConditionReasonSnapshotExecutionUnobservable = "SnapshotExecutionUnobservable"
	// ConditionReasonSnapshotExecutionNotCompleting indicates the request was sent to the storage system
	// (or is being retried) but produced no result for an unusually long time. This state can never
	// become terminal: a CSI CreateSnapshot call may be in flight.
	ConditionReasonSnapshotExecutionNotCompleting = "SnapshotExecutionNotCompleting"
	// ConditionReasonSnapshotExecutionResumed clears a previously reported stall after observable
	// activity reappeared.
	ConditionReasonSnapshotExecutionResumed = "SnapshotExecutionResumed"
)
