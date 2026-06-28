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
)

// type conditionField string
type ConditionType string
type ConditionReason string

const (
	ConditionReady          ConditionType = "Ready"
	ConditionExpired        ConditionType = "Expired"
	ConditionUploadFinished ConditionType = "UploadFinished"
	ConditionCompleted      ConditionType = "Completed"

	// Progress reasons.
	ReasonPending        ConditionReason = "Pending"
	ReasonPVCCreated     ConditionReason = "PVCCreated"
	ReasonPodReady       ConditionReason = "PodReady"
	ReasonIngressReady   ConditionReason = "IngressReady"
	ReasonExpired        ConditionReason = "Expired"
	ReasonDeleted        ConditionReason = "Deleted"
	ReasonUploadFinished ConditionReason = "UploadFinished"
	ReasonCompleted      ConditionReason = "Completed"

	// Not ready error reasons (shared by DataExport and DataImport).
	ReasonPublishFailed    ConditionReason = "PublishFailed"
	ReasonValidationFailed ConditionReason = "ValidationFailed"
	ReasonTargetNotFound   ConditionReason = "TargetNotFound"
	ReasonTargetNotReady   ConditionReason = "TargetNotReady"
	ReasonTargetFailed     ConditionReason = "TargetFailed"
	ReasonPVConflict       ConditionReason = "PVConflict"
	ReasonDeploymentFailed ConditionReason = "DeploymentFailed"
	ReasonCleanupFailed    ConditionReason = "CleanupFailed"
)

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
