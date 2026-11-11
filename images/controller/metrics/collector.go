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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// collectTimeout stops a stuck read before the default 10s Prometheus scrape timeout drops the response.
const collectTimeout = 5 * time.Second

var _ prometheus.Collector = (*leaderCollector)(nil)

// leaderCollector calls inner only on the elected replica, so the other replica neither publishes
// duplicates nor reads the cache.
type leaderCollector struct {
	inner   prometheus.Collector
	elected <-chan struct{}
}

func newLeaderCollector(inner prometheus.Collector, elected <-chan struct{}) *leaderCollector {
	return &leaderCollector{inner: inner, elected: elected}
}

// Describe sends the descriptions of inner.
func (c *leaderCollector) Describe(ch chan<- *prometheus.Desc) {
	c.inner.Describe(ch)
}

// Collect sends the metrics of inner once this replica is elected, and nothing before that.
func (c *leaderCollector) Collect(ch chan<- prometheus.Metric) {
	if !c.isLeader() {
		return
	}

	c.inner.Collect(ch)
}

// isLeader reports whether this replica has been elected.
func (c *leaderCollector) isLeader() bool {
	select {
	case <-c.elected:
		return true
	default:
		return false
	}
}

// sendGauge sends one gauge series of desc to ch. A series that cannot be built is logged and
// skipped, because an invalid metric would fail the whole scrape, controller_runtime_* included.
func sendGauge(ctx context.Context, ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labelValues ...string) {
	m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, value, labelValues...)

	if err != nil {
		log.FromContext(ctx).Error(err, "skip the series", "desc", desc.String(), "labelValues", labelValues)
		return
	}

	ch <- m
}
