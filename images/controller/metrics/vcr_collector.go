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
	// apiReader lists StorageClasses without an informer in the manager cache.
	apiReader client.Reader
	desc      *prometheus.Desc
}

type vcrKey struct {
	mode         string
	driver       string
	storageClass string
	state        string
}

func newVCRCollector(reader, apiReader client.Reader) *vcrCollector {
	return &vcrCollector{
		reader:    reader,
		apiReader: apiReader,
		desc: prometheus.NewDesc(
			"storage_foundation_vcr_objects",
			"VolumeCaptureRequests in the cluster by mode, CSI driver, StorageClass and state.",
			[]string{"mode", "driver", "storage_class", "state"},
			nil,
		),
	}
}

// Describe sends the description of the metric.
func (c *vcrCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect sends one gauge per label set.
func (c *vcrCollector) Collect(ch chan<- prometheus.Metric) {
	// prometheus.Collector.Collect has no ctx parameter.
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	requests := new(v1alpha1.VolumeCaptureRequestList)
	if err := c.reader.List(ctx, requests); err != nil {
		log.FromContext(ctx).Error(err, "list VolumeCaptureRequests")
		return
	}

	// No requests, so the StorageClasses are not needed.
	if len(requests.Items) == 0 {
		return
	}

	provisioners, err := storageClassProvisioners(ctx, c.apiReader)
	if err != nil {
		log.FromContext(ctx).Error(err, "list StorageClasses")
		return
	}

	for key, n := range c.countRequests(ctx, requests.Items, provisioners) {
		sendGauge(ctx, ch, c.desc, float64(n), key.mode, key.driver, key.storageClass, key.state)
	}
}

// countRequests counts the requests by label set. It reads the source PVC and PV of each request.
func (c *vcrCollector) countRequests(
	ctx context.Context,
	requests []v1alpha1.VolumeCaptureRequest,
	provisioners map[string]string,
) map[vcrKey]int {
	counts := make(map[vcrKey]int)

	for i := range requests {
		vcr := &requests[i]
		driver, scName := vcrVolumeLabels(ctx, c.reader, vcr, provisioners)
		key := vcrKey{
			mode:         vcrMode(vcr),
			driver:       driver,
			storageClass: scName,
			state:        vcrState(vcr),
		}

		counts[key]++
	}

	return counts
}
