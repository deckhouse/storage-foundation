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
	// ConditionReasonInvalidTTL indicates invalid TTL annotation format
	ConditionReasonInvalidTTL = "InvalidTTL"
	// ConditionReasonInternalError indicates internal error
	ConditionReasonInternalError = "InternalError"
	// ConditionReasonNotFound indicates resource not found
	ConditionReasonNotFound = "NotFound"
	// ConditionReasonRBACDenied indicates RBAC permission denied
	ConditionReasonRBACDenied = "RBACDenied"
	// ConditionReasonInvalidSource indicates invalid source specified
	ConditionReasonInvalidSource = "InvalidSource"
	// ConditionReasonPVBound indicates PV is bound and cannot be detached
	ConditionReasonPVBound = "PVBound"
	// ConditionReasonSnapshotCreationFailed indicates CSI snapshot creation failed
	ConditionReasonSnapshotCreationFailed = "SnapshotCreationFailed"
	// ConditionReasonRestoreFailed indicates restore operation failed
	ConditionReasonRestoreFailed = "RestoreFailed"
)
