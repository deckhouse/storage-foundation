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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// createdHeader is what the exposition output of the counter starts with. It is followed by the series.
const createdHeader = "# HELP storage_foundation_vcr_created_total " +
	"VolumeCaptureRequests created in the cluster by mode, CSI driver and StorageClass.\n" +
	"# TYPE storage_foundation_vcr_created_total counter\n"

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
			want: createdHeader + createdSeries(v1alpha1.VolumeCaptureModeSnapshot, testPVDriver, "fast", 1),
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
		// The StorageClass list blocks until its ctx is done, as a cache read does while the informer does not sync.
		funcs := interceptor.Funcs{
			List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}
		r, collector := newTestRecorder(t, newVolumeReader(t, funcs))
		vcr := withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data")

		start := time.Now()
		r.VCREvents().Create(t.Context(), event.CreateEvent{Object: vcr}, nil)

		if got := time.Since(start); got != eventTimeout {
			t.Errorf("VCREvents().Create() returned after %v; want %v", got, eventTimeout)
		}
		want := createdHeader + createdSeries(v1alpha1.VolumeCaptureModeSnapshot, testPVDriver, "unknown", 1)
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
			want: createdHeader + createdSeries(v1alpha1.VolumeCaptureModeSnapshot, testPVDriver, "fast", 2),
		},
		{
			name: "success: a request whose PVC is not found is counted with unknown labels",
			vcrs: []*v1alpha1.VolumeCaptureRequest{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeDetach), "missing"),
			},
			want: createdHeader + createdSeries(v1alpha1.VolumeCaptureModeDetach, "unknown", "unknown", 1),
		},
		{
			name: "error: a failed StorageClass list counts the request with an unknown StorageClass",
			vcrs: []*v1alpha1.VolumeCaptureRequest{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
			},
			funcs: failList(new(storagev1.StorageClassList)),
			want:  createdHeader + createdSeries(v1alpha1.VolumeCaptureModeSnapshot, testPVDriver, "unknown", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, collector := newTestRecorder(t, newVolumeReader(t, tt.funcs))

			for _, vcr := range tt.vcrs {
				r.countVCRCreated(t.Context(), vcr)
			}

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
func createdSeries(mode, driver, storageClass string, count int) string {
	return fmt.Sprintf("storage_foundation_vcr_created_total{driver=%q,mode=%q,storage_class=%q} %d\n",
		driver, mode, storageClass, count)
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

// newVolumeReader returns a reader that holds the PVC data, its PV of testPVDriver and the StorageClass fast.
// Its calls go through funcs.
func newVolumeReader(t *testing.T, funcs interceptor.Funcs) client.Reader {
	t.Helper()

	return newReader(t, funcs,
		newPVC("data", "fast", "pv-1"),
		newPV("pv-1", testPVDriver),
		newStorageClass("fast"),
	)
}
