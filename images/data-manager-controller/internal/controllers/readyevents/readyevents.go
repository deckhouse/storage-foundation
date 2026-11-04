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

// Package readyevents records a Kubernetes Event on a DataExport or a DataImport when its Ready
// condition moves.
package readyevents

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/deckhouse/sds-common-lib/conditions"
	"github.com/deckhouse/storage-foundation/common"
)

// Record records one Event on obj when the Ready condition in after is a transition from the one in
// before — a change of status or of reason, as conditions.RecordTransition defines it — and nothing
// otherwise. before is the status the write replaced, after the status it wrote; call it only once the
// write has gone through.
//
// The Event is Warning when the reason is a failure (common.ConditionReason.IsFailure) and Normal
// otherwise, rather than Warning on every Ready=False as the shared helper decides: here Ready=False
// also means "still on the way" (Pending, PVCCreated) and "done" (Completed, Expired).
//
// A nil rec records nothing.
func Record(rec conditions.EventRecorder, obj runtime.Object, before, after []metav1.Condition) {
	if rec == nil {
		return
	}
	cur := conditions.Get(after, string(common.ConditionReady))
	if cur == nil {
		return
	}
	conditions.RecordTransition(typedByReason{rec}, obj, conditions.Get(before, string(common.ConditionReady)), *cur)
}

// typedByReason passes Events on with the type their reason calls for.
type typedByReason struct{ rec conditions.EventRecorder }

func (t typedByReason) Eventf(regarding, related runtime.Object, _, reason, action, note string, args ...any) {
	eventType := corev1.EventTypeNormal
	if common.ConditionReason(reason).IsFailure() {
		eventType = corev1.EventTypeWarning
	}
	t.rec.Eventf(regarding, related, eventType, reason, action, note, args...)
}
