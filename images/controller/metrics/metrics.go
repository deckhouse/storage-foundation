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

// Package metrics defines the Prometheus metrics of the storage-foundation controller.
package metrics

import (
	"fmt"

	metricsstorage "github.com/deckhouse/deckhouse/pkg/metrics-storage"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Register registers the controller's metrics in reg and returns the runnable that updates the gauges
// and the recorder that writes the metrics of events. A metric has no series until the manager runs the
// runnable on the elected replica or the recorder writes an event.
func Register(reg prometheus.Registerer, reader client.Reader) (manager.Runnable, *Recorder, error) {
	st := metricsstorage.NewMetricStorage(metricsstorage.WithNewRegistry())

	// Each function registers a set of metrics in st and returns the function that updates them.
	registerFuncs := []func(*metricsstorage.MetricStorage, client.Reader) (updateFunc, error){
		registerVCRObjects,
	}

	updateFuncs := make([]updateFunc, 0, len(registerFuncs))
	for _, register := range registerFuncs {
		update, err := register(st, reader)
		if err != nil {
			return nil, nil, fmt.Errorf("register metrics: %w", err)
		}

		updateFuncs = append(updateFuncs, update)
	}

	recorder, err := newRecorder(st, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("register metrics: %w", err)
	}

	// Attached after the metrics are registered, so that a name clash fails here and not every scrape.
	if err := reg.Register(st.Collector()); err != nil {
		return nil, nil, fmt.Errorf("register metrics: %w", err)
	}

	return newUpdater(updateFuncs), recorder, nil
}
