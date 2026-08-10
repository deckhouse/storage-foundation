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

package controllers

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// stallResumedMessage is used when a previously reported stall is cleared.
const stallResumedMessage = "observable snapshot activity resumed"

// shouldEmitStallEvent reports whether the diagnosis changed in a way an administrator has to be
// told about, given the previously reported Stalled condition.
//
// Two transitions qualify: entering a stall, and switching between stall reasons. The second one
// matters as much as the first — SnapshotStackUnavailable turning into SnapshotExecutionUnobservable
// moves attention from the cluster-wide snapshot-controller to one specific CSI driver, and staying
// silent would hide exactly the actionable part. Re-reporting the same reason emits nothing, so a
// long stall does not turn into an event storm. Leaving a stall emits nothing either: it is visible
// on the condition, and it needs no action.
func shouldEmitStallEvent(previous *metav1.Condition, diagnosis stallDiagnosis) bool {
	if !diagnosis.Stalled {
		return false
	}
	if previous == nil || previous.Status != metav1.ConditionTrue {
		return true
	}
	return previous.Reason != diagnosis.Reason
}

// applyStallDiagnosis reflects a diagnosis in the Stalled condition, leaving every other condition
// untouched.
//
// SAFETY INVARIANT: a stall diagnosis is never written as the reason of the Ready condition.
// Terminality is derived as "Ready=False with a reason other than TargetsPending", so putting a
// stall reason on Ready would make a request that is still executing look finished, and the
// garbage collector would delete it — together with the ObjectKeeper that retains the snapshot.
// Ready stays False/TargetsPending for the whole diagnostic lifetime; Stalled is a separate,
// always non-terminal condition.
//
// Clearing is deliberately asymmetric: a request that never stalled gets no Stalled condition at
// all, so the condition's presence itself is the signal that something needed attention.
func applyStallDiagnosis(conditions *[]metav1.Condition, diagnosis stallDiagnosis, now metav1.Time) {
	if diagnosis.Stalled {
		// meta.SetStatusCondition (unlike setSingleCondition) keeps LastTransitionTime stable while
		// the status does not change, so the timestamp answers "since when is it stalled".
		meta.SetStatusCondition(conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionTypeStalled,
			Status:             metav1.ConditionTrue,
			Reason:             diagnosis.Reason,
			Message:            diagnosis.Message,
			LastTransitionTime: now,
		})
		return
	}

	existing := meta.FindStatusCondition(*conditions, storagev1alpha1.ConditionTypeStalled)
	if existing == nil || existing.Status != metav1.ConditionTrue {
		return
	}
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeStalled,
		Status:             metav1.ConditionFalse,
		Reason:             storagev1alpha1.ConditionReasonSnapshotExecutionResumed,
		Message:            stallResumedMessage,
		LastTransitionTime: now,
	})
}
