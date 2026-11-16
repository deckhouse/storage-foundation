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
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/controller/metrics"
)

func TestRegister(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		start bool
		want  int
	}{
		{
			name: "success: a replica that neither runs the updater nor counts an event publishes nothing",
			want: 0,
		},
		{
			name:  "success: the updater publishes the series once it runs",
			start: true,
			want:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
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
				reg := prometheus.NewPedanticRegistry()

				updater, recorder, err := metrics.Register(reg, reader)
				if err != nil {
					t.Fatalf("Register() error = %v", err)
				}
				if recorder == nil {
					t.Fatalf("Register() recorder = nil; want a recorder")
				}
				if leaderOnly, ok := updater.(manager.LeaderElectionRunnable); !ok || !leaderOnly.NeedLeaderElection() {
					t.Errorf("Register() runnable runs on every replica; want it only on the elected one")
				}
				if tt.start {
					ctx, cancel := context.WithCancel(t.Context())
					done := make(chan error, 1)
					go func() { done <- updater.Start(ctx) }()
					t.Cleanup(func() {
						cancel()
						if err := <-done; err != nil {
							t.Errorf("Start() error = %v; want nil", err)
						}
					})
					synctest.Wait()
				}

				got, err := testutil.GatherAndCount(reg)
				if err != nil {
					t.Fatalf("GatherAndCount() error = %v", err)
				}
				if got != tt.want {
					t.Errorf("GatherAndCount() = %d; want %d", got, tt.want)
				}
			})
		})
	}
}

func TestRegister_twice(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewPedanticRegistry()
	if _, _, err := metrics.Register(reg, nil); err != nil {
		t.Fatalf("Register() first call error = %v", err)
	}

	_, _, err := metrics.Register(reg, nil)
	if _, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); !ok {
		t.Errorf("Register() second call error = %v; want it to wrap %T", err, prometheus.AlreadyRegisteredError{})
	}
}

func TestRegister_nameTaken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		taken prometheus.Collector
	}{
		{
			name: "error: a gauge of the same name with other labels fails at registration",
			taken: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "storage_foundation_vcr_objects",
				Help: "Taken.",
			}, []string{"other"}),
		},
		{
			name: "error: a counter of the same name fails at registration",
			taken: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "storage_foundation_vcr_objects",
				Help: "Taken.",
			}),
		},
		{
			name: "error: a counter of the name of the created requests counter fails at registration",
			taken: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "storage_foundation_vcr_created_total",
				Help: "Taken.",
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := prometheus.NewPedanticRegistry()
			if err := reg.Register(tt.taken); err != nil {
				t.Fatalf("Register(taken) error = %v", err)
			}

			// Registration reads nothing, so no reader is needed.
			if _, _, err := metrics.Register(reg, nil); err == nil {
				t.Errorf("Register() error = nil; want an error for the taken name")
			}
		})
	}
}
