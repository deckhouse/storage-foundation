/*
Copyright 2026 Flant JSC

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

package metrics

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// vcrMode returns the mode of vcr, or unknown for a value other than the two the API defines,
// because spec.mode is user input and must not put arbitrary strings into a label.
func vcrMode(vcr *v1alpha1.VolumeCaptureRequest) string {
	mode := vcr.Spec.Mode

	if mode == v1alpha1.VolumeCaptureModeSnapshot || mode == v1alpha1.VolumeCaptureModeDetach {
		return vcr.Spec.Mode
	}

	return labelUnknown
}

// vcrState returns the state label of vcr from its Ready condition. The state is completed or failed
// exactly when isVolumeCaptureTerminal in internal/controllers reports the request as terminal.
func vcrState(vcr *v1alpha1.VolumeCaptureRequest) string {
	ready := meta.FindStatusCondition(vcr.Status.Conditions, v1alpha1.ConditionTypeReady)

	switch {
	case ready == nil:
		return statePending
	case ready.Status == metav1.ConditionTrue:
		return stateCompleted
	case ready.Status == metav1.ConditionFalse && ready.Reason != v1alpha1.ConditionReasonTargetsPending:
		return stateFailed
	}

	return statePending
}

// vcrStage returns the stage label of a pending vcr from its deletion timestamp and Ready condition.
func vcrStage(vcr *v1alpha1.VolumeCaptureRequest) string {
	ready := meta.FindStatusCondition(vcr.Status.Conditions, v1alpha1.ConditionTypeReady)

	switch {
	case vcr.DeletionTimestamp != nil:
		return stageOther
	case ready == nil || ready.Status == metav1.ConditionUnknown:
		return stageNotStarted
	case ready.Status == metav1.ConditionFalse && ready.Reason == v1alpha1.ConditionReasonTargetsPending:
		return stageSnapshotInProgress
	}

	return stageOther
}
