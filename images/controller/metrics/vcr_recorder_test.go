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
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	metricsstorage "github.com/deckhouse/deckhouse/pkg/metrics-storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// createdHeader is what the exposition output of the counter starts with. It is followed by the series.
const createdHeader = "# HELP storage_foundation_vcr_created_total " +
	"VolumeCaptureRequests created in the cluster by namespace, mode, CSI driver, StorageClass " +
	"and VolumeSnapshotClass.\n" +
	"# TYPE storage_foundation_vcr_created_total counter\n"

// durationHeader is what the exposition output of the histogram starts with. It is followed by the series.
const durationHeader = "# HELP storage_foundation_vcr_duration_seconds " +
	"Time from the creation of a VolumeCaptureRequest to its final status by namespace, mode, CSI driver, " +
	"StorageClass, VolumeSnapshotClass and result.\n" +
	"# TYPE storage_foundation_vcr_duration_seconds histogram\n"

func TestRecorder_VCREvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		obj             client.Object
		isInInitialList bool
		want            string
	}{
		{
			name: "success: a created request is counted",
			obj:  withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
			want: createdHeader + createdSeries(v1alpha1.VolumeCaptureModeSnapshot, boundVolume, 1),
		},
		{
			name:            "success: a request from the initial list is not counted",
			obj:             withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
			isInInitialList: true,
		},
		{
			name: "success: an object that is not a request is not counted",
			obj:  newPVC("data", "fast", "pv-1"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, collector := newTestRecorder(t, newVolumeReader(t, interceptor.Funcs{}))

			r.VCREvents().Create(t.Context(), event.CreateEvent{Object: tt.obj, IsInInitialList: tt.isInInitialList}, nil)

			if err := testutil.CollectAndCompare(collector, strings.NewReader(tt.want)); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestRecorder_VCREvents_timeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r, collector := newTestRecorder(t, newVolumeReader(t, blockedListFuncs()))
		vcr := withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data")

		start := time.Now()
		r.VCREvents().Create(t.Context(), event.CreateEvent{Object: vcr}, nil)

		if got := time.Since(start); got != eventTimeout {
			t.Errorf("VCREvents().Create() returned after %v; want %v", got, eventTimeout)
		}
		want := createdHeader + createdSeries(v1alpha1.VolumeCaptureModeSnapshot,
			vcrVolume{driver: testPVDriver, storageClass: "unknown", snapshotClass: "unknown"}, 1)
		if err := testutil.CollectAndCompare(collector, strings.NewReader(want)); err != nil {
			t.Errorf("CollectAndCompare() error = %v", err)
		}
	})
}

func TestRecorder_VCREvents_update(t *testing.T) {
	t.Parallel()

	completed := ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)
	failed := ready(metav1.ConditionFalse, v1alpha1.ConditionReasonNotFound)
	inProgress := ready(metav1.ConditionFalse, v1alpha1.ConditionReasonTargetsPending)

	tests := []struct {
		name   string
		oldObj client.Object
		newObj client.Object
		want   string
	}{
		{
			name:   "success: a request that completes is observed with result completed",
			oldObj: createdVCR(inProgress),
			newObj: finishedVCR(completed, 7*time.Second),
			want:   durationHeader + durationSeries(v1alpha1.VolumeCaptureModeSnapshot, boundVolume, "completed", 7),
		},
		{
			name:   "success: a request that fails is observed with result failed",
			oldObj: createdVCR(),
			newObj: finishedVCR(failed, 3*time.Second),
			want:   durationHeader + durationSeries(v1alpha1.VolumeCaptureModeSnapshot, boundVolume, "failed", 3),
		},
		{
			name:   "success: a completion time before the creation time is observed as zero",
			oldObj: createdVCR(),
			newObj: finishedVCR(failed, -time.Second),
			want:   durationHeader + durationSeries(v1alpha1.VolumeCaptureModeSnapshot, boundVolume, "failed", 0),
		},
		{
			name:   "success: an update of a request that is already final is not observed",
			oldObj: finishedVCR(completed, 7*time.Second),
			newObj: finishedVCR(completed, 7*time.Second),
		},
		{
			name:   "success: an update of a request that stays pending is not observed",
			oldObj: createdVCR(),
			newObj: createdVCR(inProgress),
		},
		{
			name:   "success: an update whose old object is not a request is not observed",
			oldObj: newPVC("data", "fast", "pv-1"),
			newObj: finishedVCR(completed, 7*time.Second),
		},
		{
			name:   "success: an update whose new object is not a request is not observed",
			oldObj: createdVCR(),
			newObj: newPVC("data", "fast", "pv-1"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, collector := newTestRecorder(t, newVolumeReader(t, interceptor.Funcs{}))

			r.VCREvents().Update(t.Context(), event.UpdateEvent{ObjectOld: tt.oldObj, ObjectNew: tt.newObj}, nil)

			err := testutil.CollectAndCompare(collector, strings.NewReader(tt.want), "storage_foundation_vcr_duration_seconds")
			if err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestRecorder_VCREvents_updateTimeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r, collector := newTestRecorder(t, newVolumeReader(t, blockedListFuncs()))
		completed := ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)
		e := event.UpdateEvent{ObjectOld: createdVCR(), ObjectNew: finishedVCR(completed, 7*time.Second)}

		start := time.Now()
		r.VCREvents().Update(t.Context(), e, nil)

		if got := time.Since(start); got != eventTimeout {
			t.Errorf("VCREvents().Update() returned after %v; want %v", got, eventTimeout)
		}
		want := durationHeader + durationSeries(v1alpha1.VolumeCaptureModeSnapshot,
			vcrVolume{driver: testPVDriver, storageClass: "unknown", snapshotClass: "unknown"}, "completed", 7)
		if err := testutil.CollectAndCompare(collector, strings.NewReader(want)); err != nil {
			t.Errorf("CollectAndCompare() error = %v", err)
		}
	})
}

func TestRecorder_countVCRCreated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		vcrs  []*v1alpha1.VolumeCaptureRequest
		funcs interceptor.Funcs
		want  string
	}{
		{
			name: "success: requests with the same labels make one series with their count",
			vcrs: []*v1alpha1.VolumeCaptureRequest{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				withTarget(newVCR("b", v1alpha1.VolumeCaptureModeSnapshot), "data"),
			},
			want: createdHeader + createdSeries(v1alpha1.VolumeCaptureModeSnapshot, boundVolume, 2),
		},
		{
			name: "success: a Detach request without its PVC is counted with unknown labels and no VolumeSnapshotClass",
			vcrs: []*v1alpha1.VolumeCaptureRequest{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeDetach), "missing"),
			},
			want: createdHeader + createdSeries(v1alpha1.VolumeCaptureModeDetach, unknownDetachVolume, 1),
		},
		{
			name: "error: a failed StorageClass list counts the request with an unknown StorageClass",
			vcrs: []*v1alpha1.VolumeCaptureRequest{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
			},
			funcs: failList(new(storagev1.StorageClassList)),
			want: createdHeader + createdSeries(v1alpha1.VolumeCaptureModeSnapshot,
				vcrVolume{driver: testPVDriver, storageClass: "unknown", snapshotClass: "unknown"}, 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, collector := newTestRecorder(t, newVolumeReader(t, tt.funcs))

			for _, vcr := range tt.vcrs {
				r.countVCRCreated(vcr, r.resolveVCRVolume(t.Context(), vcr))
			}

			if err := testutil.CollectAndCompare(collector, strings.NewReader(tt.want)); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestRecorder_NamespaceEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		deleted string
		want    string
	}{
		{
			name:    "success: a deleted namespace loses its series and another namespace keeps its own",
			deleted: testNamespace,
			want: createdHeader +
				relabelNamespace(createdSeries(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, 1), "other") +
				durationHeader +
				relabelNamespace(durationSeries(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "completed", 7), "other"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, collector := newTestRecorder(t, newReader(t, interceptor.Funcs{}))
			completed := ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)
			for _, ns := range []string{testNamespace, "other"} {
				vcr := inNamespace(finishedVCR(completed, 7*time.Second), ns)
				r.countVCRCreated(vcr, unknownVolume)
				r.observeVCRDuration(vcr, unknownVolume)
			}

			// The watch of the controller delivers only the metadata of a Namespace.
			deleted := &metav1.PartialObjectMetadata{Name: tt.deleted}
			r.NamespaceEvents().Delete(t.Context(), event.DeleteEvent{Object: deleted}, nil)

			if err := testutil.CollectAndCompare(collector, strings.NewReader(tt.want)); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestNewRecorder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		gauge   string
		wantErr bool
	}{
		{
			name:  "success: a gauge of another name leaves the name free",
			gauge: "storage_foundation_other",
		},
		{
			name:    "error: a gauge of the same name takes the name",
			gauge:   "storage_foundation_vcr_created_total",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st := metricsstorage.NewMetricStorage(metricsstorage.WithNewRegistry())
			if _, err := st.RegisterGauge(tt.gauge, nil); err != nil {
				t.Fatalf("RegisterGauge(%q) error = %v", tt.gauge, err)
			}

			r, err := newRecorder(st, newReader(t, interceptor.Funcs{}))

			if (err != nil) != tt.wantErr {
				t.Fatalf("newRecorder() error = %v; wantErr %t", err, tt.wantErr)
			}
			if (r == nil) != tt.wantErr {
				t.Errorf("newRecorder() recorder is nil = %t; want %t", r == nil, tt.wantErr)
			}
		})
	}
}

// createdSeries returns the exposition line of one series of the counter, labels in the order Prometheus prints them.
func createdSeries(mode string, volume vcrVolume, count int) string {
	return fmt.Sprintf("storage_foundation_vcr_created_total"+
		"{driver=%q,mode=%q,namespace=%q,storage_class=%q,volume_snapshot_class=%q} %d\n",
		volume.driver, mode, testNamespace, volume.storageClass, volume.snapshotClass, count)
}

// durationSeries returns the exposition lines of one series of the histogram that holds the single observation
// seconds, labels in the order Prometheus prints them.
func durationSeries(mode string, volume vcrVolume, result string, seconds float64) string {
	const name = "storage_foundation_vcr_duration_seconds"
	labels := fmt.Sprintf("driver=%q,mode=%q,namespace=%q,result=%q,storage_class=%q,volume_snapshot_class=%q",
		volume.driver, mode, testNamespace, result, volume.storageClass, volume.snapshotClass)

	var b strings.Builder
	for _, bound := range durationBuckets() {
		count := 0
		if seconds <= bound {
			count = 1
		}
		fmt.Fprintf(&b, name+`_bucket{%s,le="%g"} %d`+"\n", labels, bound, count)
	}
	fmt.Fprintf(&b, name+`_bucket{%s,le="+Inf"} 1`+"\n", labels)
	fmt.Fprintf(&b, name+`_sum{%s} %g`+"\n", labels, seconds)
	fmt.Fprintf(&b, name+`_count{%s} 1`+"\n", labels)

	return b.String()
}

// createdVCR returns a Snapshot request of the PVC data made by newPVC, created at a fixed time, with conditions.
func createdVCR(conditions ...metav1.Condition) *v1alpha1.VolumeCaptureRequest {
	vcr := withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, conditions...), "data")
	vcr.CreationTimestamp = metav1.NewTime(time.Date(2026, time.November, 17, 10, 0, 0, 0, time.UTC))

	return vcr
}

// finishedVCR returns the request of createdVCR with the final condition cond, completed after d.
func finishedVCR(cond metav1.Condition, d time.Duration) *v1alpha1.VolumeCaptureRequest {
	vcr := createdVCR(cond)
	vcr.Status.CompletionTimestamp = new(metav1.NewTime(vcr.CreationTimestamp.Add(d)))

	return vcr
}

// blockedListFuncs returns funcs whose List blocks until its ctx is done, as a cache read does while the informer
// does not sync.
func blockedListFuncs() interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
}

// newTestRecorder registers the recorder in a new storage and returns it and the collector of that storage.
func newTestRecorder(t *testing.T, reader client.Reader) (*Recorder, prometheus.Collector) {
	t.Helper()

	st := metricsstorage.NewMetricStorage(metricsstorage.WithNewRegistry())
	r, err := newRecorder(st, reader)
	if err != nil {
		t.Fatalf("newRecorder() error = %v", err)
	}

	return r, st.Collector()
}

// newVolumeReader returns a reader that holds the PVC data, its PV of testPVDriver and the StorageClass fast that
// names testSnapshotClass.
// Its calls go through funcs.
func newVolumeReader(t *testing.T, funcs interceptor.Funcs) client.Reader {
	t.Helper()

	return newReader(t, funcs,
		newPVC("data", "fast", "pv-1"),
		newPV("pv-1", testPVDriver),
		newStorageClass("fast"),
	)
}
