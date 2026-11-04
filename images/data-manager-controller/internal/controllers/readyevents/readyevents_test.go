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

package readyevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// drain returns what the fake recorder has received so far.
func drain(rec *events.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-rec.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func ready(status metav1.ConditionStatus, reason common.ConditionReason, msg string) []metav1.Condition {
	return []metav1.Condition{{Type: string(common.ConditionReady), Status: status, Reason: string(reason), Message: msg}}
}

// A DataExport goes through the reasons below in a typical life. Each step records an Event only when
// Ready changes status or reason, and its type follows the reason: the progress steps and the normal
// endings are Normal although Ready is False, the failures are Warning.
func TestRecordFollowsReadyAcrossALifecycle(t *testing.T) {
	steps := []struct {
		name  string
		after []metav1.Condition
		want  []string
	}{
		{"created", ready(metav1.ConditionFalse, common.ReasonPending, "Started"), []string{"Normal Pending Started"}},
		{"same verdict", ready(metav1.ConditionFalse, common.ReasonPending, "Started"), nil},
		{"waits for the target", ready(metav1.ConditionFalse, common.ReasonTargetNotReady, "not bound yet"), []string{"Normal TargetNotReady not bound yet"}},
		{"waits with another message", ready(metav1.ConditionFalse, common.ReasonTargetNotReady, "no volumeMode yet"), nil},
		{"breaks", ready(metav1.ConditionFalse, common.ReasonDeploymentFailed, "deployment is stuck"), []string{"Warning DeploymentFailed deployment is stuck"}},
		{"serves", ready(metav1.ConditionTrue, common.ReasonServerReady, "ready"), []string{"Normal ServerReady ready"}},
		{"serves again", ready(metav1.ConditionTrue, common.ReasonServerReady, "ready"), nil},
		{"expires", ready(metav1.ConditionFalse, common.ReasonExpired, "idle timeout expired"), []string{"Normal Expired idle timeout expired"}},
	}

	rec := events.NewFakeRecorder(10)
	obj := &dev1alpha1.DataExport{ObjectMeta: metav1.ObjectMeta{Name: "de", Namespace: "ns"}}
	var before []metav1.Condition
	for _, s := range steps {
		Record(rec, obj, before, s.after)
		assert.Equal(t, s.want, drain(rec), s.name)
		before = s.after
	}
}

// A DataImport ends with Ready=False as well: Completed on success, TargetFailed on a failed import.
func TestRecordTypesTheEndingsOfAnImport(t *testing.T) {
	serving := ready(metav1.ConditionTrue, common.ReasonServerReady, "ready")
	obj := &dev1alpha1.DataImport{ObjectMeta: metav1.ObjectMeta{Name: "di", Namespace: "ns"}}

	rec := events.NewFakeRecorder(10)
	Record(rec, obj, serving, ready(metav1.ConditionFalse, common.ReasonCompleted, "Data import completed"))
	assert.Equal(t, []string{"Normal Completed Data import completed"}, drain(rec))

	Record(rec, obj, serving, ready(metav1.ConditionFalse, common.ReasonTargetFailed, "capture failed"))
	assert.Equal(t, []string{"Warning TargetFailed capture failed"}, drain(rec))
}

// Only Ready is watched: a write that moves another condition, or has no Ready at all, records nothing,
// and neither does a nil recorder.
func TestRecordIgnoresAllButReady(t *testing.T) {
	obj := &dev1alpha1.DataImport{ObjectMeta: metav1.ObjectMeta{Name: "di", Namespace: "ns"}}
	serving := ready(metav1.ConditionTrue, common.ReasonServerReady, "ready")
	uploaded := append(ready(metav1.ConditionTrue, common.ReasonServerReady, "ready"), metav1.Condition{
		Type: string(common.ConditionUploadFinished), Status: metav1.ConditionTrue, Reason: string(common.ReasonUploadFinished),
	})

	rec := events.NewFakeRecorder(10)
	Record(rec, obj, serving, uploaded)
	Record(rec, obj, nil, nil)
	assert.Empty(t, drain(rec))

	assert.NotPanics(t, func() {
		Record(nil, obj, nil, ready(metav1.ConditionFalse, common.ReasonCleanupFailed, "boom"))
	})
}
