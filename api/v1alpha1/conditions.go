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
// Only Ready condition is used - it is set to True on success or False on final failure
const (
	ConditionTypeReady = "Ready"
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
