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

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Register registers the controller's collectors in reg; they publish only after elected is closed.
// apiReader reads StorageClasses straight from the API server.
func Register(reg prometheus.Registerer, reader, apiReader client.Reader, elected <-chan struct{}) error {
	collectors := []prometheus.Collector{
		newVCRCollector(reader, apiReader),
	}

	for _, c := range collectors {
		if err := reg.Register(newLeaderCollector(c, elected)); err != nil {
			return fmt.Errorf("register metrics: %w", err)
		}
	}

	return nil
}
