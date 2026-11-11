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

package metrics_test

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/controller/metrics"
)

func TestRegister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		elected       bool
		registrations int
		want          int
		wantErr       bool
	}{
		{
			name:          "success: a replica that has not been elected publishes nothing",
			registrations: 1,
			want:          0,
		},
		{
			name:          "success: the elected leader publishes the series",
			elected:       true,
			registrations: 1,
			want:          1,
		},
		{
			name:          "error: a second registration into the same registry fails",
			registrations: 2,
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, storagev1.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatalf("add to scheme: %v", err)
				}
			}
			vcr := &v1alpha1.VolumeCaptureRequest{
				Name:      "a",
				Namespace: "ns",
				Spec:      v1alpha1.VolumeCaptureRequestSpec{Mode: v1alpha1.VolumeCaptureModeSnapshot},
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vcr).Build()
			elected := make(chan struct{})
			if tt.elected {
				close(elected)
			}
			reg := prometheus.NewPedanticRegistry()

			var err error
			for range tt.registrations {
				err = metrics.Register(reg, reader, reader, elected)
			}

			if (err != nil) != tt.wantErr {
				t.Fatalf("Register() error = %v; wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr {
				if alreadyRegistered, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); !ok {
					t.Errorf("Register() error = %v; want it to wrap %T", err, alreadyRegistered)
				}
				return
			}

			got, err := testutil.GatherAndCount(reg)
			if err != nil {
				t.Fatalf("GatherAndCount() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("GatherAndCount() = %d; want %d", got, tt.want)
			}
		})
	}
}
