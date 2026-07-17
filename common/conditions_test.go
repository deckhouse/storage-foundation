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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

func TestPhaseIsTerminal(t *testing.T) {
	terminal := []Phase{PhaseCompleted, PhaseExpired, PhaseFailed}
	nonTerminal := []Phase{PhasePending, PhaseReady, PhaseTerminating, Phase("")}

	for _, p := range terminal {
		assert.True(t, p.IsTerminal(), "%s must be terminal", p)
	}
	for _, p := range nonTerminal {
		assert.False(t, p.IsTerminal(), "%s must NOT be terminal", p)
	}
}

// TestStripConditionsNotIn is the load-bearing legacy migration: existing DataImport/DataExport objects
// carry a stale condition (type "Expired") that the narrowed CRD condition-type enum no longer permits;
// the controller must strip it (and any other out-of-catalog type) before the first status write, or every
// Status().Update fails enum validation (status.conditions is an atomic list; ratcheting is absent on
// older Kubernetes).
func TestStripConditionsNotIn(t *testing.T) {
	c := func(typ string) metav1.Condition {
		return metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: "X", LastTransitionTime: metav1.Now()}
	}

	t.Run("removes stale Expired, keeps DataImport catalog", func(t *testing.T) {
		status := &dev1alpha1.DataExportImportStatus{Conditions: []metav1.Condition{
			c("Ready"), c("Expired"), c("UploadFinished"), c("Completed"),
		}}
		changed := StripConditionsNotIn(status, ConditionReady, ConditionUploadFinished, ConditionCompleted)
		assert.True(t, changed)
		types := []string{}
		for _, cond := range status.Conditions {
			types = append(types, cond.Type)
		}
		assert.ElementsMatch(t, []string{"Ready", "UploadFinished", "Completed"}, types)
	})

	t.Run("DataExport keeps only Ready", func(t *testing.T) {
		status := &dev1alpha1.DataExportImportStatus{Conditions: []metav1.Condition{
			c("Ready"), c("Expired"),
		}}
		changed := StripConditionsNotIn(status, ConditionReady)
		assert.True(t, changed)
		assert.Len(t, status.Conditions, 1)
		assert.Equal(t, "Ready", status.Conditions[0].Type)
	})

	t.Run("no change when all conditions are in the catalog", func(t *testing.T) {
		status := &dev1alpha1.DataExportImportStatus{Conditions: []metav1.Condition{c("Ready")}}
		assert.False(t, StripConditionsNotIn(status, ConditionReady, ConditionUploadFinished, ConditionCompleted))
		assert.Len(t, status.Conditions, 1)
	})

	t.Run("nil / empty status is safe", func(t *testing.T) {
		assert.False(t, StripConditionsNotIn(nil, ConditionReady))
		assert.False(t, StripConditionsNotIn(&dev1alpha1.DataExportImportStatus{}, ConditionReady))
	})
}

// TestSetCompletionTimestampOnce verifies the GC retention clock is stamped exactly once, at the first
// terminal transition, and never on non-terminal phases.
func TestSetCompletionTimestampOnce(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC))
	later := metav1.NewTime(now.Add(time.Hour))

	t.Run("stamps on first terminal", func(t *testing.T) {
		status := &dev1alpha1.DataExportImportStatus{}
		assert.True(t, SetCompletionTimestampOnce(status, PhaseCompleted, now))
		assert.Equal(t, now.Time, status.CompletionTimestamp.Time)
	})

	t.Run("does not re-stamp when already set", func(t *testing.T) {
		status := &dev1alpha1.DataExportImportStatus{CompletionTimestamp: &now}
		assert.False(t, SetCompletionTimestampOnce(status, PhaseExpired, later))
		assert.Equal(t, now.Time, status.CompletionTimestamp.Time)
	})

	t.Run("no-op on non-terminal phase", func(t *testing.T) {
		for _, p := range []Phase{PhasePending, PhaseReady, PhaseTerminating} {
			status := &dev1alpha1.DataExportImportStatus{}
			assert.False(t, SetCompletionTimestampOnce(status, p, now))
			assert.Nil(t, status.CompletionTimestamp)
		}
	})

	t.Run("nil status is safe", func(t *testing.T) {
		assert.False(t, SetCompletionTimestampOnce(nil, PhaseCompleted, now))
	})
}
