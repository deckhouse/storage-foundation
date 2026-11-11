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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// header is what the exposition output of the metric starts with. It is followed by the series.
const header = "# HELP storage_foundation_vcr_objects VolumeCaptureRequests in the cluster by mode and state.\n" +
	"# TYPE storage_foundation_vcr_objects gauge\n"

var errList = errors.New("list failed")

func TestVCRCollector_Collect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		requests []client.Object
		want     string
	}{
		{
			name: "success: no requests publish no series",
		},
		{
			name: "success: Ready=True is completed",
			requests: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "completed", 1),
		},
		{
			name: "success: Ready=False with a final reason is failed",
			requests: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionFalse, v1alpha1.ConditionReasonNotFound)),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "failed", 1),
		},
		{
			name: "success: a request without the Ready condition is pending",
			requests: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "pending", 1),
		},
		{
			name: "success: Ready=False with the TargetsPending reason is pending",
			requests: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionFalse, v1alpha1.ConditionReasonTargetsPending)),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "pending", 1),
		},
		{
			name: "success: Ready=Unknown is pending",
			requests: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionUnknown, "Progressing")),
			},
			want: header + series(v1alpha1.VolumeCaptureModeSnapshot, "pending", 1),
		},
		{
			name: "success: an invalid mode becomes unknown instead of leaking into the label",
			requests: []client.Object{
				newVCR("a", "Bogus"),
			},
			want: header + series("unknown", "pending", 1),
		},
		{
			name: "success: requests with the same labels make one series with their count",
			requests: []client.Object{
				newVCR("a", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
				newVCR("b", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
				newVCR("c", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
				newVCR("d", v1alpha1.VolumeCaptureModeSnapshot, ready(metav1.ConditionFalse, v1alpha1.ConditionReasonNotFound)),
				newVCR("e", v1alpha1.VolumeCaptureModeDetach, ready(metav1.ConditionTrue, v1alpha1.ConditionReasonCompleted)),
			},
			want: header +
				series(v1alpha1.VolumeCaptureModeDetach, "completed", 1) +
				series(v1alpha1.VolumeCaptureModeSnapshot, "completed", 3) +
				series(v1alpha1.VolumeCaptureModeSnapshot, "failed", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newVCRCollector(newReader(t, interceptor.Funcs{}, tt.requests...))

			if err := testutil.CollectAndCompare(c, strings.NewReader(tt.want)); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

func TestVCRCollector_Collect_listError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		funcs interceptor.Funcs
	}{
		{
			name: "error: a failed list publishes nothing instead of a stale or partial number",
			funcs: interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errList
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			vcr := newVCR("a", v1alpha1.VolumeCaptureModeSnapshot)
			c := newVCRCollector(newReader(t, tt.funcs, vcr))

			if err := testutil.CollectAndCompare(c, strings.NewReader("")); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
		})
	}
}

// series returns the exposition line of one series of the metric.
func series(mode, state string, count int) string {
	return fmt.Sprintf("storage_foundation_vcr_objects{mode=%q,state=%q} %d\n", mode, state, count)
}

// newReader returns a fake client holding objects. Its calls go through funcs.
func newReader(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(funcs).
		Build()
}

func newVCR(name, mode string, conditions ...metav1.Condition) *v1alpha1.VolumeCaptureRequest {
	return &v1alpha1.VolumeCaptureRequest{
		Name:      name,
		Namespace: "ns",
		Spec:      v1alpha1.VolumeCaptureRequestSpec{Mode: mode},
		Status:    v1alpha1.VolumeCaptureRequestStatus{Conditions: conditions},
	}
}

func ready(status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: v1alpha1.ConditionTypeReady, Status: status, Reason: reason}
}
