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

package gc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

var gcNow = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

const gcTTL = 24 * time.Hour

func di(name string, phase common.Phase, completion *time.Time, deleting bool) *dev1alpha1.DataImport {
	obj := &dev1alpha1.DataImport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status:     dev1alpha1.DataExportImportStatus{Phase: string(phase)},
	}
	if completion != nil {
		t := metav1.NewTime(*completion)
		obj.Status.CompletionTimestamp = &t
	}
	if deleting {
		t := metav1.NewTime(gcNow.Add(-time.Minute))
		obj.DeletionTimestamp = &t
		obj.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
	}
	return obj
}

func de(name string, phase common.Phase, completion *time.Time, deleting bool) *dev1alpha1.DataExport {
	obj := &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status:     dev1alpha1.DataExportImportStatus{Phase: string(phase)},
	}
	if completion != nil {
		t := metav1.NewTime(*completion)
		obj.Status.CompletionTimestamp = &t
	}
	if deleting {
		t := metav1.NewTime(gcNow.Add(-time.Minute))
		obj.DeletionTimestamp = &t
		obj.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
	}
	return obj
}

func ptr(t time.Time) *time.Time { return &t }

// TestIsReapable covers the shared terminal+age+fail-safe logic used by both GC managers.
func TestIsReapable(t *testing.T) {
	old := gcNow.Add(-25 * time.Hour)  // older than TTL
	fresh := gcNow.Add(-1 * time.Hour) // younger than TTL

	tests := []struct {
		name   string
		status dev1alpha1.DataExportImportStatus
		delTS  bool
		want   bool
	}{
		{"terminal Completed, old → reap", statusOf(common.PhaseCompleted, &old), false, true},
		{"terminal Expired, old → reap", statusOf(common.PhaseExpired, &old), false, true},
		{"terminal Failed, old → reap", statusOf(common.PhaseFailed, &old), false, true},
		{"terminal but fresh → keep", statusOf(common.PhaseCompleted, &fresh), false, false},
		{"non-terminal Pending, old completion → keep", statusOf(common.PhasePending, &old), false, false},
		{"non-terminal Ready, old completion → keep", statusOf(common.PhaseReady, &old), false, false},
		{"terminal old but terminating → keep", statusOf(common.PhaseExpired, &old), true, false},
		{"terminal old but nil completionTimestamp → keep (fail-safe)", statusOf(common.PhaseCompleted, nil), false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &dev1alpha1.DataImport{Status: tt.status}
			if tt.delTS {
				ts := metav1.NewTime(gcNow.Add(-time.Minute))
				obj.DeletionTimestamp = &ts
				obj.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
			}
			assert.Equal(t, tt.want, isReapable(obj, &obj.Status, gcTTL, gcNow))
		})
	}
}

func statusOf(phase common.Phase, completion *time.Time) dev1alpha1.DataExportImportStatus {
	s := dev1alpha1.DataExportImportStatus{Phase: string(phase)}
	if completion != nil {
		t := metav1.NewTime(*completion)
		s.CompletionTimestamp = &t
	}
	return s
}

func TestDataImportGCManager_ShouldBeDeleted(t *testing.T) {
	m := &dataImportGCManager{ttl: gcTTL, now: func() time.Time { return gcNow }}

	old := gcNow.Add(-25 * time.Hour)
	assert.True(t, m.ShouldBeDeleted(di("old-completed", common.PhaseCompleted, &old, false)))
	assert.False(t, m.ShouldBeDeleted(di("pending", common.PhasePending, &old, false)))
	assert.False(t, m.ShouldBeDeleted(di("fresh", common.PhaseExpired, ptr(gcNow.Add(-time.Hour)), false)))
	// A non-DataImport object is never reaped by this manager.
	assert.False(t, m.ShouldBeDeleted(de("wrong-type", common.PhaseExpired, &old, false)))
}

func TestDataExportGCManager_ShouldBeDeleted(t *testing.T) {
	m := &dataExportGCManager{ttl: gcTTL, now: func() time.Time { return gcNow }}

	old := gcNow.Add(-25 * time.Hour)
	assert.True(t, m.ShouldBeDeleted(de("old-expired", common.PhaseExpired, &old, false)))
	assert.True(t, m.ShouldBeDeleted(de("old-failed", common.PhaseFailed, &old, false)))
	assert.False(t, m.ShouldBeDeleted(de("ready", common.PhaseReady, &old, false)))
	assert.False(t, m.ShouldBeDeleted(di("wrong-type", common.PhaseCompleted, &old, false)))
}

// TestDataImportGCManager_ListForDelete drives the full list→filter path against a fake client: only the
// terminal object older than TTL is selected; Pending/InProgress, fresh terminal, and terminating are left.
func TestDataImportGCManager_ListForDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dev1alpha1.AddToScheme(scheme))

	old := gcNow.Add(-25 * time.Hour)
	fresh := gcNow.Add(-time.Hour)

	objs := []client.Object{
		di("reap-me", common.PhaseCompleted, &old, false),
		di("still-pending", common.PhasePending, &old, false),
		di("still-running", common.PhaseReady, &old, false),
		di("too-fresh", common.PhaseExpired, &fresh, false),
		di("already-deleting", common.PhaseFailed, &old, true),
		di("no-completion-ts", common.PhaseCompleted, nil, false),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	m := &dataImportGCManager{client: c, ttl: gcTTL, now: func() time.Time { return gcNow }}

	got, err := m.ListForDelete(context.Background(), gcNow)
	require.NoError(t, err)

	gotNames := make([]string, 0, len(got))
	for _, o := range got {
		gotNames = append(gotNames, o.GetName())
	}
	assert.ElementsMatch(t, []string{"reap-me"}, gotNames,
		"only the terminal DataImport older than TTL (with a completionTimestamp, not terminating) is selected")
}

func TestDataExportGCManager_ListForDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, dev1alpha1.AddToScheme(scheme))

	old := gcNow.Add(-25 * time.Hour)

	objs := []client.Object{
		de("reap-expired", common.PhaseExpired, &old, false),
		de("reap-failed", common.PhaseFailed, &old, false),
		de("serving", common.PhaseReady, &old, false),
		de("pending", common.PhasePending, &old, false),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	m := &dataExportGCManager{client: c, ttl: gcTTL, now: func() time.Time { return gcNow }}

	got, err := m.ListForDelete(context.Background(), gcNow)
	require.NoError(t, err)

	gotNames := make([]string, 0, len(got))
	for _, o := range got {
		gotNames = append(gotNames, o.GetName())
	}
	assert.ElementsMatch(t, []string{"reap-expired", "reap-failed"}, gotNames)
}

// TestCompletionTime pins the AgeTimeFunc: nil timestamp → zero time (so getAge yields 0 and never expires).
func TestCompletionTime(t *testing.T) {
	assert.True(t, completionTime(&dev1alpha1.DataExportImportStatus{}).IsZero())
	ct := metav1.NewTime(gcNow)
	assert.Equal(t, gcNow, completionTime(&dev1alpha1.DataExportImportStatus{CompletionTimestamp: &ct}))
}

// TestIsReapable_StrictTTLBoundary pins the strict inequality in the dm-GC predicate (independent of the
// common-filter boundary test): age must EXCEED ttl, so an object exactly at the TTL boundary is retained.
func TestIsReapable_StrictTTLBoundary(t *testing.T) {
	atBoundary := gcNow.Add(-gcTTL)             // now - ct == ttl → not > ttl → keep
	justOver := gcNow.Add(-gcTTL - time.Second) // now - ct == ttl+1s → reap
	assert.False(t, isReapable(di("at", common.PhaseCompleted, &atBoundary, false), &di("at", common.PhaseCompleted, &atBoundary, false).Status, gcTTL, gcNow))
	assert.True(t, isReapable(di("over", common.PhaseCompleted, &justOver, false), &di("over", common.PhaseCompleted, &justOver, false).Status, gcTTL, gcNow))
}
