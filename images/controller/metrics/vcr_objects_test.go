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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	metricsstorage "github.com/deckhouse/deckhouse/pkg/metrics-storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// header is what the exposition output of the metric starts with. It is followed by the series.
const header = "# HELP storage_foundation_vcr_objects " +
	"VolumeCaptureRequests in the cluster by mode, CSI driver, StorageClass, VolumeSnapshotClass and state.\n" +
	"# TYPE storage_foundation_vcr_objects gauge\n"

// pendingHeader is what the exposition output of the pending metric starts with.
const pendingHeader = "# HELP storage_foundation_vcr_pending_objects " +
	"Pending VolumeCaptureRequests in the cluster by mode, CSI driver, StorageClass, VolumeSnapshotClass, " +
	"age and stage.\n" +
	"# TYPE storage_foundation_vcr_pending_objects gauge\n"

// The PV driver and the StorageClass provisioner differ, so a test sees where the driver came from.
const (
	testPVDriver    = "pv.csi.example.com"
	testProvisioner = "sc.csi.example.com"
)

// testSnapshotClass is the VolumeSnapshotClass that newStorageClass names.
const testSnapshotClass = "fast-snapshots"

// testSnapshotClassAnnotation is spelled out, so that a typo in snapshotClassAnnotation fails the tests.
const testSnapshotClassAnnotation = "storage.deckhouse.io/volumesnapshotclass"

// Volume labels that many tests expect.
var (
	// unknownVolume is the labels of a Snapshot request whose source PVC cannot be read.
	unknownVolume = vcrVolume{driver: "unknown", storageClass: "unknown", snapshotClass: "unknown"}
	// unknownDetachVolume is the labels of a Detach request whose source PVC cannot be read.
	unknownDetachVolume = vcrVolume{driver: "unknown", storageClass: "unknown", snapshotClass: "none"}
	// boundVolume is the labels of a request whose PVC is bound to a PV of testPVDriver and has the StorageClass fast.
	boundVolume = vcrVolume{driver: testPVDriver, storageClass: "fast", snapshotClass: testSnapshotClass}
)

var errRead = errors.New("read failed")

// bubbleStart is the time a synctest bubble starts at.
var bubbleStart = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

func TestVCRObjectsMetric_update(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		objects []client.Object
		want    string
	}{
		{
			name: "success: no requests publish no series",
		},
		{
			name: "success: Ready=True is completed",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "completed", 1),
		},
		{
			name: "success: Ready=False with a final reason is failed",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionFalse, v1alpha1.ConditionReasonNotFound)),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "failed", 1),
		},
		{
			name: "success: a request without the Ready condition is pending",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "pending", 1),
		},
		{
			name: "success: Ready=False with the TargetsPending reason is pending",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionFalse, v1alpha1.ConditionReasonTargetsPending)),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "pending", 1),
		},
		{
			name: "success: Ready=Unknown is pending",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionUnknown, "Progressing")),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "pending", 1),
		},
		{
			name: "success: an invalid mode becomes unknown instead of leaking into the label",
			objects: []client.Object{
				newVCR("a", "Bogus"),
			},
			want: header + series("unknown", unknownVolume, "pending", 1),
		},
		{
			name: "success: requests with the same labels make one series with their count",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
				newVCR("b", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
				newVCR("c", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
				newVCR("d", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionFalse, v1alpha1.ConditionReasonNotFound)),
				newVCR("e", v1alpha1.VolumeCaptureModeDetach, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
			},
			want: header +
				series(v1alpha1.VolumeCaptureModeDetach, unknownDetachVolume, "completed", 1) +
				series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "completed", 3) +
				series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "failed", 1),
		},
		{
			name: "success: a bound PVC gives the driver of its PV, its StorageClass and its VolumeSnapshotClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, boundVolume, "pending", 1),
		},
		{
			name: "success: an unbound PVC gives the provisioner of its StorageClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", ""),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot,
				vcrVolume{driver: testProvisioner, storageClass: "fast", snapshotClass: testSnapshotClass}, "pending", 1),
		},
		{
			name: "success: a PVC whose PV is gone gives the provisioner of its StorageClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot,
				vcrVolume{driver: testProvisioner, storageClass: "fast", snapshotClass: testSnapshotClass}, "pending", 1),
		},
		{
			name: "success: a PV that is not CSI has an unknown driver",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", ""),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot,
				vcrVolume{driver: "unknown", storageClass: "fast", snapshotClass: testSnapshotClass}, "pending", 1),
		},
		{
			name: "success: a bound PVC without storageClassName has the driver of its PV and an unknown StorageClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "", "pv-1"),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot,
				vcrVolume{driver: testPVDriver, storageClass: "unknown", snapshotClass: "unknown"}, "pending", 1),
		},
		{
			name: "success: an unbound PVC whose StorageClass does not exist has unknown labels",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "gone", ""),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "pending", 1),
		},
		{
			name: "success: a request whose PVC is gone has unknown labels",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "pending", 1),
		},
		{
			name: "success: a PVC recreated under the same name is not the source PVC and gives unknown labels",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				recreated(newPVC("data", "fast", "pv-1")),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "pending", 1),
		},
		{
			name: "success: a StorageClass without the annotation names no VolumeSnapshotClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", testPVDriver),
				withAnnotations(newStorageClass("fast"), nil),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot,
				vcrVolume{driver: testPVDriver, storageClass: "fast", snapshotClass: "none"}, "pending", 1),
		},
		{
			name: "success: a StorageClass with an empty annotation names no VolumeSnapshotClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", testPVDriver),
				withAnnotations(newStorageClass("fast"), map[string]string{testSnapshotClassAnnotation: ""}),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot,
				vcrVolume{driver: testPVDriver, storageClass: "fast", snapshotClass: "none"}, "pending", 1),
		},
		{
			name: "success: a Detach request has no VolumeSnapshotClass even if its StorageClass names one",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeDetach), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeDetach,
				vcrVolume{driver: testPVDriver, storageClass: "fast", snapshotClass: "none"}, "pending", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			update, collector := registerTestVCRObjects(t, newReader(t, interceptor.Funcs{}, tt.objects...))

			update(t.Context())

			err := testutil.CollectAndCompare(collector, strings.NewReader(tt.want), "storage_foundation_vcr_objects")
			if err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestVCRObjectsMetric_update_readErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		funcs interceptor.Funcs
		want  string
	}{
		{
			name:  "error: a failed VolumeCaptureRequest list publishes nothing instead of a stale or partial number",
			funcs: failList(new(v1alpha1.VolumeCaptureRequestList)),
		},
		{
			name:  "error: a failed StorageClass list publishes nothing instead of unknown labels",
			funcs: failList(new(storagev1.StorageClassList)),
		},
		{
			name:  "error: a failed PVC read keeps the request with unknown labels",
			funcs: failGet(new(corev1.PersistentVolumeClaim)),
			want:  header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "pending", 1),
		},
		{
			name:  "error: a failed PV read keeps the request with an unknown driver",
			funcs: failGet(new(corev1.PersistentVolume)),
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot,
				vcrVolume{driver: "unknown", storageClass: "fast", snapshotClass: testSnapshotClass}, "pending", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			update, collector := registerTestVCRObjects(t, newReader(t, tt.funcs,
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			))

			update(t.Context())

			err := testutil.CollectAndCompare(collector, strings.NewReader(tt.want), "storage_foundation_vcr_objects")
			if err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestVCRObjectsMetric_update_twice(t *testing.T) {
	t.Parallel()

	// A request without a creation time is older than a day.
	pending := header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "pending", 1) +
		pendingHeader + pendingSeries(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "gt_24h", "not_started", 1)

	tests := []struct {
		name   string
		first  client.Reader
		second client.Reader
		want   string
	}{
		{
			name:   "success: a request that stays keeps its series",
			first:  newReader(t, interceptor.Funcs{}, newVCR("a", v1alpha1.VolumeCaptureModeSnapshot)),
			second: newReader(t, interceptor.Funcs{}, newVCR("a", v1alpha1.VolumeCaptureModeSnapshot)),
			want:   pending,
		},
		{
			name: "success: the series of a deleted request disappears",
			first: newReader(t, interceptor.Funcs{},
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot),
				newVCR("b", v1alpha1.VolumeCaptureModeDetach),
			),
			second: newReader(t, interceptor.Funcs{}, newVCR("a", v1alpha1.VolumeCaptureModeSnapshot)),
			want:   pending,
		},
		{
			name:  "success: a request that completed leaves its pending series",
			first: newReader(t, interceptor.Funcs{}, newVCR("a", v1alpha1.VolumeCaptureModeSnapshot)),
			second: newReader(t, interceptor.Funcs{},
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
			),
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "completed", 1),
		},
		{
			name:  "error: a failed read after a successful update drops the series",
			first: newReader(t, interceptor.Funcs{}, newVCR("a", v1alpha1.VolumeCaptureModeSnapshot)),
			second: newReader(t, failList(new(v1alpha1.VolumeCaptureRequestList)),
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot),
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := &readerSwitch{Reader: tt.first}
			update, collector := registerTestVCRObjects(t, reader)

			update(t.Context())
			reader.Reader = tt.second
			update(t.Context())

			if err := testutil.CollectAndCompare(collector, strings.NewReader(tt.want)); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestVCRObjectsMetric_update_pending(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		objects []client.Object
		want    string
	}{
		{
			name: "success: no requests publish no series",
		},
		{
			name: "success: completed and failed requests are not pending",
			objects: []client.Object{
				createdAgo(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)), time.Minute),
				createdAgo(newVCR("b", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionFalse, v1alpha1.ConditionReasonNotFound)), time.Minute),
			},
		},
		{
			name: "success: a request without the Ready condition is not_started",
			objects: []client.Object{
				createdAgo(newVCR("a", v1alpha1.VolumeCaptureModeDetach), time.Minute),
			},
			want: pendingHeader + pendingSeries(v1alpha1.VolumeCaptureModeDetach,
				unknownDetachVolume, "lt_3m", "not_started", 1),
		},
		{
			name: "success: Ready=Unknown is not_started",
			objects: []client.Object{
				createdAgo(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionUnknown, "Progressing")), 20*time.Minute),
			},
			want: pendingHeader + pendingSeries(v1alpha1.VolumeCaptureModeSnapshot,
				unknownVolume, "15m_30m", "not_started", 1),
		},
		{
			name: "success: Ready=False with the TargetsPending reason is snapshot_in_progress",
			objects: []client.Object{
				pendingVCR("a", 5*time.Minute),
			},
			want: pendingHeader + pendingSeries(v1alpha1.VolumeCaptureModeSnapshot,
				unknownVolume, "3m_15m", "snapshot_in_progress", 1),
		},
		{
			name: "success: a request being deleted is other whatever its Ready condition",
			objects: []client.Object{
				deleting(pendingVCR("a", time.Hour)),
			},
			want: pendingHeader + pendingSeries(v1alpha1.VolumeCaptureModeSnapshot,
				unknownVolume, "30m_2h", "other", 1),
		},
		{
			name: "success: pending requests with the same labels make one series with their count",
			objects: []client.Object{
				pendingVCR("a", 5*time.Minute),
				pendingVCR("b", 10*time.Minute),
				pendingVCR("c", 3*time.Hour),
			},
			want: pendingHeader +
				pendingSeries(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "2h_24h", "snapshot_in_progress", 1) +
				pendingSeries(v1alpha1.VolumeCaptureModeSnapshot, unknownVolume, "3m_15m", "snapshot_in_progress", 2),
		},
		{
			name: "success: a pending request has the volume labels of its source PVC",
			objects: []client.Object{
				withTarget(pendingVCR("a", 25*time.Hour), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			},
			want: pendingHeader + pendingSeries(v1alpha1.VolumeCaptureModeSnapshot,
				boundVolume, "gt_24h", "snapshot_in_progress", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The bubble starts its clock at bubbleStart, so the age of a request is known.
			synctest.Test(t, func(t *testing.T) {
				update, collector := registerTestVCRObjects(t, newReader(t, interceptor.Funcs{}, tt.objects...))

				update(t.Context())

				err := testutil.CollectAndCompare(collector, strings.NewReader(tt.want), "storage_foundation_vcr_pending_objects")
				if err != nil {
					t.Errorf("CollectAndCompare() error = %v", err)
				}
			})
		})
	}
}

func TestRegisterVCRObjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		counter string
		wantErr bool
	}{
		{
			name:    "success: a counter of another name leaves the name free",
			counter: "storage_foundation_other_total",
		},
		{
			name:    "error: a counter of the same name takes the name",
			counter: "storage_foundation_vcr_objects",
			wantErr: true,
		},
		{
			name:    "error: a counter of the pending metric name takes the name",
			counter: "storage_foundation_vcr_pending_objects",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st := metricsstorage.NewMetricStorage(metricsstorage.WithNewRegistry())
			if _, err := st.RegisterCounter(tt.counter, nil); err != nil {
				t.Fatalf("RegisterCounter(%q) error = %v", tt.counter, err)
			}

			update, err := registerVCRObjects(st, newReader(t, interceptor.Funcs{}))

			if (err != nil) != tt.wantErr {
				t.Fatalf("registerVCRObjects() error = %v; wantErr %t", err, tt.wantErr)
			}
			if (update == nil) != tt.wantErr {
				t.Errorf("registerVCRObjects() update is nil = %t; want %t", update == nil, tt.wantErr)
			}
		})
	}
}

// readerSwitch reads through the reader in it, so that a test can change the cluster between two updates.
type readerSwitch struct {
	client.Reader
}

// series returns the exposition line of one series of the metric, labels in the order Prometheus prints them.
func series(mode string, volume vcrVolume, state string, count int) string {
	return fmt.Sprintf(
		"storage_foundation_vcr_objects{driver=%q,mode=%q,state=%q,storage_class=%q,volume_snapshot_class=%q} %d\n",
		volume.driver, mode, state, volume.storageClass, volume.snapshotClass, count)
}

// pendingSeries returns the exposition line of one series of the pending metric, labels in the order
// Prometheus prints them.
func pendingSeries(mode string, volume vcrVolume, age, stage string, count int) string {
	return fmt.Sprintf("storage_foundation_vcr_pending_objects"+
		"{age=%q,driver=%q,mode=%q,stage=%q,storage_class=%q,volume_snapshot_class=%q} %d\n",
		age, volume.driver, mode, stage, volume.storageClass, volume.snapshotClass, count)
}

// registerTestVCRObjects registers the metrics in a new storage and returns its update function and the
// collector of that storage.
func registerTestVCRObjects(t *testing.T, reader client.Reader) (updateFunc, prometheus.Collector) {
	t.Helper()

	st := metricsstorage.NewMetricStorage(metricsstorage.WithNewRegistry())
	update, err := registerVCRObjects(st, reader)
	if err != nil {
		t.Fatalf("registerVCRObjects() error = %v", err)
	}

	return update, st.Collector()
}

// newReader returns a fake client holding objects. Its calls go through funcs.
func newReader(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, corev1.AddToScheme, storagev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(funcs).
		Build()
}

// failList returns funcs that fail every List of the type of list.
func failList(list client.ObjectList) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if reflect.TypeOf(l) == reflect.TypeOf(list) {
				return errRead
			}
			return c.List(ctx, l, opts...)
		},
	}
}

// failGet returns funcs that fail every Get of the type of obj.
func failGet(obj client.Object) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if reflect.TypeOf(o) == reflect.TypeOf(obj) {
				return errRead
			}
			return c.Get(ctx, key, o, opts...)
		},
	}
}

func newVCR(name, mode string, conditions ...metav1.Condition) *v1alpha1.VolumeCaptureRequest {
	return &v1alpha1.VolumeCaptureRequest{
		Name:      name,
		Namespace: "ns",
		Spec:      v1alpha1.VolumeCaptureRequestSpec{Mode: mode},
		Status:    v1alpha1.VolumeCaptureRequestStatus{Conditions: conditions},
	}
}

// createdAgo sets the creation time of vcr to age before bubbleStart and returns vcr.
func createdAgo(vcr *v1alpha1.VolumeCaptureRequest, age time.Duration) *v1alpha1.VolumeCaptureRequest {
	vcr.CreationTimestamp = metav1.NewTime(bubbleStart.Add(-age))
	return vcr
}

// pendingVCR returns a Snapshot request with Ready=False and the TargetsPending reason, created age before
// bubbleStart.
func pendingVCR(name string, age time.Duration) *v1alpha1.VolumeCaptureRequest {
	vcr := newVCR(name, v1alpha1.VolumeCaptureModeSnapshot,
		ready(metav1.ConditionFalse, v1alpha1.ConditionReasonTargetsPending))
	return createdAgo(vcr, age)
}

// deleting marks vcr as being deleted and returns it. The finalizer keeps the fake client from refusing it.
func deleting(vcr *v1alpha1.VolumeCaptureRequest) *v1alpha1.VolumeCaptureRequest {
	vcr.DeletionTimestamp = new(metav1.NewTime(bubbleStart))
	vcr.Finalizers = []string{"test"}
	return vcr
}

// withTarget points the spec.target of vcr at the PVC name made by newPVC and returns vcr.
func withTarget(vcr *v1alpha1.VolumeCaptureRequest, name string) *v1alpha1.VolumeCaptureRequest {
	vcr.Spec.Target = &v1alpha1.VolumeCaptureTarget{
		UID:        string(pvcUID(name)),
		APIVersion: "v1",
		Kind:       v1alpha1.KindPVC,
		Name:       name,
	}
	return vcr
}

func ready(status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: v1alpha1.ConditionTypeReady, Status: status, Reason: reason}
}

// newPVC returns a PVC in the namespace of newVCR. An empty class leaves storageClassName unset, and an
// empty volume leaves the PVC unbound.
func newPVC(name, class, volume string) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		Name:      name,
		Namespace: "ns",
		UID:       pvcUID(name),
		Spec:      corev1.PersistentVolumeClaimSpec{VolumeName: volume},
	}
	if class != "" {
		pvc.Spec.StorageClassName = &class
	}

	return pvc
}

// recreated gives pvc another UID, as if it were deleted and created again under the same name.
func recreated(pvc *corev1.PersistentVolumeClaim) *corev1.PersistentVolumeClaim {
	pvc.UID = "recreated"
	return pvc
}

func pvcUID(name string) types.UID {
	return types.UID("uid-" + name)
}

// newPV returns a PV of the CSI driver, or a PV that is not CSI when driver is empty.
func newPV(name, driver string) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{Name: name}
	if driver != "" {
		pv.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: name}
	}

	return pv
}

// newStorageClass returns a StorageClass of testProvisioner that names testSnapshotClass.
func newStorageClass(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		Name:        name,
		Annotations: map[string]string{testSnapshotClassAnnotation: testSnapshotClass},
		Provisioner: testProvisioner,
	}
}

// withAnnotations replaces the annotations of sc and returns sc.
func withAnnotations(sc *storagev1.StorageClass, annotations map[string]string) *storagev1.StorageClass {
	sc.Annotations = annotations
	return sc
}
