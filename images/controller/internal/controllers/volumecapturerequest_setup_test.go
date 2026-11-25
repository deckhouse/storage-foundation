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

package controllers

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/controller/metrics"
)

// subscribeTimeout bounds the wait for the controller to subscribe to the informer of the requests.
const subscribeTimeout = 10 * time.Second

func TestVolumeCaptureRequestController_SetupWithManager_countsCreated(t *testing.T) {
	t.Parallel()

	informer, reg := setupWithFakeInformer(t)

	informer.Add(&storagev1alpha1.VolumeCaptureRequest{
		Name:      "a",
		Namespace: "ns",
		Spec:      storagev1alpha1.VolumeCaptureRequestSpec{Mode: storagev1alpha1.VolumeCaptureModeSnapshot},
	})

	want := "# HELP storage_foundation_vcr_created_total " +
		"VolumeCaptureRequests created in the cluster by namespace, mode, CSI driver, StorageClass " +
		"and VolumeSnapshotClass.\n" +
		"# TYPE storage_foundation_vcr_created_total counter\n" +
		`storage_foundation_vcr_created_total{driver="unknown",mode="Snapshot",namespace="ns",storage_class="unknown",` +
		`volume_snapshot_class="unknown"} 1` + "\n"
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "storage_foundation_vcr_created_total"); err != nil {
		t.Errorf("GatherAndCompare() error = %v", err)
	}
}

func TestVolumeCaptureRequestController_SetupWithManager_observesDuration(t *testing.T) {
	t.Parallel()

	informer, reg := setupWithFakeInformer(t)

	pending := &storagev1alpha1.VolumeCaptureRequest{
		Name:              "a",
		Namespace:         "ns",
		CreationTimestamp: metav1.NewTime(time.Date(2026, time.November, 17, 10, 0, 0, 0, time.UTC)),
		Spec:              storagev1alpha1.VolumeCaptureRequestSpec{Mode: storagev1alpha1.VolumeCaptureModeSnapshot},
	}
	completed := pending.DeepCopy()
	completed.Status.Conditions = []metav1.Condition{{
		Type:   storagev1alpha1.ConditionTypeReady,
		Status: metav1.ConditionTrue,
		Reason: storagev1alpha1.ConditionReasonCompleted,
	}}
	completed.Status.CompletionTimestamp = new(metav1.NewTime(pending.CreationTimestamp.Add(7 * time.Second)))
	informer.Update(pending, completed)

	got, err := testutil.GatherAndCount(reg, "storage_foundation_vcr_duration_seconds")
	if err != nil {
		t.Fatalf("GatherAndCount() error = %v", err)
	}
	if got != 1 {
		t.Errorf("GatherAndCount() = %d series of storage_foundation_vcr_duration_seconds; want 1", got)
	}
}

func TestVolumeCaptureRequestController_SetupWithManager_nilMetrics(t *testing.T) {
	t.Parallel()

	// The scheme knows the requests, so that only the missing recorder can fail the setup.
	scheme := newTestScheme(t)
	mgr := newTestManager(t, scheme, &informertest.FakeInformers{Scheme: scheme})
	r := new(VolumeCaptureRequestController)

	if err := r.SetupWithManager(mgr); err == nil {
		t.Errorf("SetupWithManager() with nil Metrics error = nil; want an error")
	}
}

// setupWithFakeInformer sets up the controller with metrics on a manager whose informer of the requests is fake,
// starts the manager and waits until For and Watches subscribe to that informer. It returns the informer and the
// registry of the metrics.
func setupWithFakeInformer(t *testing.T) (*subscribedInformer, *prometheus.Registry) {
	t.Helper()

	scheme := newTestScheme(t)
	informer := &subscribedInformer{
		FakeInformer: new(controllertest.FakeInformer),
		subscribed:   make(chan struct{}, 2), //nolint:gocritic // room for the handlers of For and Watches, so a subscription never blocks
	}
	mgr := newTestManager(t, scheme, &notFoundCache{FakeInformers: &informertest.FakeInformers{
		Scheme: scheme,
		InformersByGVK: map[schema.GroupVersionKind]toolscache.SharedIndexInformer{
			storagev1alpha1.GroupVersion.WithKind("VolumeCaptureRequest"): informer,
		},
	}})

	reg := prometheus.NewPedanticRegistry()
	_, recorder, err := metrics.Register(reg, fake.NewClientBuilder().WithScheme(scheme).Build())
	if err != nil {
		t.Fatalf("metrics.Register() error = %v", err)
	}
	r := &VolumeCaptureRequestController{Client: mgr.GetClient(), Scheme: scheme, Metrics: recorder}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager() error = %v", err)
	}
	startManager(t, mgr)

	// For and Watches each subscribe a handler; an event is sent only after both are in place.
	for i := range 2 {
		select {
		case <-informer.subscribed:
		case <-time.After(subscribeTimeout):
			t.Fatalf("the controller subscribed %d handlers to the VolumeCaptureRequest informer; want 2", i)
		}
	}

	return informer, reg
}

// subscribedInformer is a fake informer that reports every handler subscribed to it.
type subscribedInformer struct {
	*controllertest.FakeInformer

	subscribed chan struct{}

	// mu guards the handlers of FakeInformer, which the sources of the controller subscribe concurrently.
	mu sync.Mutex
}

func (i *subscribedInformer) AddEventHandlerWithOptions(
	h toolscache.ResourceEventHandler,
	opts toolscache.HandlerOptions,
) (toolscache.ResourceEventHandlerRegistration, error) {
	i.mu.Lock()
	registration, err := i.FakeInformer.AddEventHandlerWithOptions(h, opts)
	i.mu.Unlock()
	if err != nil {
		return nil, err
	}
	i.subscribed <- struct{}{}

	return registration, nil
}

// notFoundCache is a fake cache without objects, so that Reconcile of an event returns without calling the API server.
type notFoundCache struct {
	*informertest.FakeInformers
}

func (c *notFoundCache) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return apierrors.NewNotFound(schema.GroupResource{}, key.Name)
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	// corev1 is for the Namespace watch that drops the metric series of a deleted namespace.
	schemeBuilder := runtime.NewSchemeBuilder(storagev1alpha1.AddToScheme, corev1.AddToScheme)
	if err := schemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}

	return scheme
}

// newTestManager returns a manager on c that never reaches an API server.
func newTestManager(t *testing.T, scheme *runtime.Scheme, c cache.Cache) manager.Manager {
	t.Helper()

	mgr, err := manager.New(&rest.Config{Host: "https://127.0.0.1:1"}, manager.Options{
		Scheme:                 scheme,
		NewCache:               func(*rest.Config, cache.Options) (cache.Cache, error) { return c, nil },
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Every run of a test builds a controller of the same name.
		Controller: ctrlconfig.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatalf("manager.New() error = %v", err)
	}

	return mgr
}

// startManager runs mgr until the test ends.
func startManager(t *testing.T, mgr manager.Manager) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start() error = %v; want nil", err)
		}
	})
}
