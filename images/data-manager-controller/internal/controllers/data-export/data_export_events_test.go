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

package dataexport

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// drainEvents returns what the fake recorder has received so far.
func drainEvents(rec *events.FakeRecorder) []string {
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

func withReady(de *dev1alpha1.DataExport, status metav1.ConditionStatus, reason common.ConditionReason, msg string) *dev1alpha1.DataExport {
	out := de.DeepCopy()
	meta.SetStatusCondition(&out.Status.Conditions, metav1.Condition{
		Type: string(common.ConditionReady), Status: status, Reason: string(reason), Message: msg,
	})
	return out
}

// The status write records an Event when it moves Ready and only then: a later pass that repeats the
// verdict, re-words it or changes another status field must not add to the DataExport's Events.
func TestUpdateDataExport_RecordsReadyTransitions(t *testing.T) {
	ctx := context.Background()
	stored := createDataExport(dev1alpha1.DataExportSpec{TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindPVC, Name: "test-pvc"}})
	cl := newFakeClientWithStatus(t, setupTestScheme(), stored)
	rec := events.NewFakeRecorder(10)
	r := createTestReconciler(cl, cl, createTestConfig())
	r.Recorder = rec

	pending := withReady(stored, metav1.ConditionFalse, common.ReasonPending, "Started")
	failed := withReady(pending, metav1.ConditionFalse, common.ReasonTargetNotFound, "user PVC test-ns/test-pvc: not found")
	failedAgain := withReady(failed, metav1.ConditionFalse, common.ReasonTargetNotFound, "user PVC test-ns/test-pvc: still not found")
	serving := withReady(failedAgain, metav1.ConditionTrue, common.ReasonServerReady, "ready")
	servingWithPhase := serving.DeepCopy()
	servingWithPhase.Status.Phase = string(common.PhaseReady)

	steps := []struct {
		name     string
		old, new *dev1alpha1.DataExport
		want     []string
	}{
		{"first verdict", stored, pending, []string{"Normal Pending Started"}},
		{"breaks", pending, failed, []string{"Warning TargetNotFound user PVC test-ns/test-pvc: not found"}},
		{"fails with another message", failed, failedAgain, nil},
		{"recovers", failedAgain, serving, []string{"Normal ServerReady ready"}},
		{"another status field", serving, servingWithPhase, nil},
		{"nothing changes", servingWithPhase, servingWithPhase, nil},
	}
	for _, s := range steps {
		require.NoError(t, r.updateDataExport(ctx, s.old, s.new), s.name)
		assert.Equal(t, s.want, drainEvents(rec), s.name)
	}
}

// A status write that does not land records nothing: the transition it would announce did not happen.
func TestUpdateDataExport_FailedWriteRecordsNothing(t *testing.T) {
	stored := createDataExport(dev1alpha1.DataExportSpec{TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindPVC, Name: "test-pvc"}})
	cl := fake.NewClientBuilder().WithScheme(setupTestScheme()).
		WithObjects(stored).WithStatusSubresource(&dev1alpha1.DataExport{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return errors.New("apiserver unavailable")
			},
		}).Build()
	rec := events.NewFakeRecorder(10)
	r := createTestReconciler(cl, cl, createTestConfig())
	r.Recorder = rec

	err := r.updateDataExport(context.Background(), stored, withReady(stored, metav1.ConditionFalse, common.ReasonDeploymentFailed, "stuck"))
	require.Error(t, err)
	assert.Empty(t, drainEvents(rec))
}
