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

package dataimport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// The status write records an Event when it moves Ready and only then. A DataImport ends with
// Ready=False/Completed, which is a success and is recorded as Normal; an UploadFinished that turns True
// on its own is not recorded.
func TestUpdateDataImport_RecordsReadyTransitions(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	stored := &dev1alpha1.DataImport{ObjectMeta: metav1.ObjectMeta{Name: "imp", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(stored).WithObjects(stored).Build()
	rec := events.NewFakeRecorder(10)

	next := func(prev *dev1alpha1.DataImport, conds ...metav1.Condition) *dev1alpha1.DataImport {
		out := prev.DeepCopy()
		for _, c := range conds {
			meta.SetStatusCondition(&out.Status.Conditions, c)
		}
		return out
	}
	pending := next(stored, cond(common.ConditionReady, metav1.ConditionFalse, common.ReasonPending))
	pvcCreated := next(pending, cond(common.ConditionReady, metav1.ConditionFalse, common.ReasonPVCCreated))
	serving := next(pvcCreated, cond(common.ConditionReady, metav1.ConditionTrue, common.ReasonServerReady))
	uploaded := next(serving, cond(common.ConditionUploadFinished, metav1.ConditionTrue, common.ReasonUploadFinished))
	completed := next(uploaded, cond(common.ConditionReady, metav1.ConditionFalse, common.ReasonCompleted))

	steps := []struct {
		name     string
		old, new *dev1alpha1.DataImport
		want     []string
	}{
		{"first verdict", stored, pending, []string{"Normal Pending "}},
		{"PVC created", pending, pvcCreated, []string{"Normal PVCCreated "}},
		{"serves", pvcCreated, serving, []string{"Normal ServerReady "}},
		{"upload finished", serving, uploaded, nil},
		{"completes", uploaded, completed, []string{"Normal Completed "}},
		{"nothing changes", completed, completed, nil},
	}
	for _, s := range steps {
		require.NoError(t, updateDataImport(ctx, c, rec, s.old, s.new), s.name)
		assert.Equal(t, s.want, drainEvents(rec), s.name)
	}
}
