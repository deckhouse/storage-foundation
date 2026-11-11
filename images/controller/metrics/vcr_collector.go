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

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

var _ prometheus.Collector = (*vcrCollector)(nil)

// vcrCollector counts VolumeCaptureRequests from the cache on every scrape.
type vcrCollector struct {
	reader client.Reader
	desc   *prometheus.Desc
}

type vcrKey struct {
	mode  string
	state string
}

func newVCRCollector(reader client.Reader) *vcrCollector {
	return &vcrCollector{
		reader: reader,
		desc: prometheus.NewDesc(
			"storage_foundation_vcr_objects",
			"VolumeCaptureRequests in the cluster by mode and state.",
			[]string{"mode", "state"},
			nil,
		),
	}
}

// Describe sends the description of the metric.
func (c *vcrCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect sends one gauge per label set. It sends nothing when the requests cannot be read:
// a gap in the scrape is visible, a wrong number is not.
func (c *vcrCollector) Collect(ch chan<- prometheus.Metric) {
	// Collect has no context parameter, so a scrape sets its own deadline.
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	requests := new(v1alpha1.VolumeCaptureRequestList)
	if err := c.reader.List(ctx, requests); err != nil {
		log.FromContext(ctx).Error(err, "list VolumeCaptureRequests")
		return
	}

	for key, n := range countRequests(requests.Items) {
		sendGauge(ctx, ch, c.desc, float64(n), key.mode, key.state)
	}
}

// countRequests counts the requests by label set.
func countRequests(requests []v1alpha1.VolumeCaptureRequest) map[vcrKey]int {
	counts := make(map[vcrKey]int)

	for i := range requests {
		counts[vcrKey{mode: vcrMode(&requests[i]), state: vcrState(&requests[i])}]++
	}

	return counts
}
