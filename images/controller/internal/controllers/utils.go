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
)

// setSingleCondition sets a condition, removing any existing condition of the same type first
func setSingleCondition(conds *[]metav1.Condition, cond metav1.Condition) {
	meta.RemoveStatusCondition(conds, cond.Type)
	meta.SetStatusCondition(conds, cond)
}

// isConditionTrue checks if a condition of the given type is True
func isConditionTrue(conditions []metav1.Condition, conditionType string) bool {
	cond := meta.FindStatusCondition(conditions, conditionType)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// isConditionFalse checks if a condition of the given type is False
func isConditionFalse(conditions []metav1.Condition, conditionType string) bool {
	cond := meta.FindStatusCondition(conditions, conditionType)
	return cond != nil && cond.Status == metav1.ConditionFalse
}

// isTerminal checks if a resource is in terminal state (Ready=True or Ready=False).
// Terminal resources are immutable and should not be processed further.
func isTerminal(conditions []metav1.Condition, conditionType string) bool {
	cond := meta.FindStatusCondition(conditions, conditionType)
	return cond != nil && (cond.Status == metav1.ConditionTrue || cond.Status == metav1.ConditionFalse)
}
