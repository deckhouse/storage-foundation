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
	"VolumeCaptureRequests in the cluster by mode, CSI driver, StorageClass and state.\n" +
	"# TYPE storage_foundation_vcr_objects gauge\n"

// The PV driver and the StorageClass provisioner differ, so a test sees where the driver came from.
const (
	testPVDriver    = "pv.csi.example.com"
	testProvisioner = "sc.csi.example.com"
)

var errRead = errors.New("read failed")

func TestVCRCollector_Collect(t *testing.T) {
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
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "completed", 1),
		},
		{
			name: "success: Ready=False with a final reason is failed",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionFalse, v1alpha1.ConditionReasonNotFound)),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "failed", 1),
		},
		{
			name: "success: a request without the Ready condition is pending",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "pending", 1),
		},
		{
			name: "success: Ready=False with the TargetsPending reason is pending",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionFalse, v1alpha1.ConditionReasonTargetsPending)),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "pending", 1),
		},
		{
			name: "success: Ready=Unknown is pending",
			objects: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionUnknown, "Progressing")),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "pending", 1),
		},
		{
			name: "success: an invalid mode becomes unknown instead of leaking into the label",
			objects: []client.Object{
				newVCR("a", "Bogus"),
			},
			want: header + series("unknown", "unknown", "unknown", "pending", 1),
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
				series(v1alpha1.VolumeCaptureModeDetach, "unknown", "unknown", "completed", 1) +
				series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "completed", 3) +
				series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "failed", 1),
		},
		{
			name: "success: a bound PVC gives the driver of its PV and its StorageClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, testPVDriver, "fast", "pending", 1),
		},
		{
			name: "success: an unbound PVC gives the provisioner of its StorageClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", ""),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, testProvisioner, "fast", "pending", 1),
		},
		{
			name: "success: a PVC whose PV is gone gives the provisioner of its StorageClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, testProvisioner, "fast", "pending", 1),
		},
		{
			name: "success: a PV that is not CSI has an unknown driver",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", ""),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "fast", "pending", 1),
		},
		{
			name: "success: a bound PVC without storageClassName has the driver of its PV and an unknown StorageClass",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "", "pv-1"),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, testPVDriver, "unknown", "pending", 1),
		},
		{
			name: "success: an unbound PVC whose StorageClass does not exist has unknown labels",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "gone", ""),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "pending", 1),
		},
		{
			name: "success: a request whose PVC is gone has unknown labels",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "pending", 1),
		},
		{
			name: "success: a PVC recreated under the same name is not the source PVC and gives unknown labels",
			objects: []client.Object{
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				recreated(newPVC("data", "fast", "pv-1")),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "pending", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newReader(t, interceptor.Funcs{}, tt.objects...)
			c := newVCRCollector(r, r)

			if err := testutil.CollectAndCompare(c, strings.NewReader(tt.want)); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestVCRCollector_Collect_readErrors(t *testing.T) {
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
			want:  header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "unknown", "pending", 1),
		},
		{
			name:  "error: a failed PV read keeps the request with an unknown driver",
			funcs: failGet(new(corev1.PersistentVolume)),
			want:  header + series(v1alpha1.VolumeCaptureModeSnapshot, "unknown", "fast", "pending", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newReader(t, tt.funcs,
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", testPVDriver),
				newStorageClass("fast"),
			)
			c := newVCRCollector(r, r)

			if err := testutil.CollectAndCompare(c, strings.NewReader(tt.want)); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestVCRCollector_Collect_apiReader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
	}{
		{
			name: "success: StorageClasses are listed through apiReader, not the cache",
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, testPVDriver, "fast", "pending", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := newReader(t, failList(new(storagev1.StorageClassList)),
				withTarget(newVCR("a", v1alpha1.VolumeCaptureModeSnapshot), "data"),
				newPVC("data", "fast", "pv-1"),
				newPV("pv-1", testPVDriver),
			)
			apiReader := newReader(t, interceptor.Funcs{}, newStorageClass("fast"))
			c := newVCRCollector(reader, apiReader)

			if err := testutil.CollectAndCompare(c, strings.NewReader(tt.want)); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

// series returns the exposition line of one series of the metric, labels in the order Prometheus prints them.
func series(mode, driver, storageClass, state string, count int) string {
	return fmt.Sprintf("storage_foundation_vcr_objects{driver=%q,mode=%q,state=%q,storage_class=%q} %d\n",
		driver, mode, state, storageClass, count)
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

func newStorageClass(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{Name: name, Provisioner: testProvisioner}
}
