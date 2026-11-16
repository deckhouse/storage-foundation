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
	"fmt"

	metricsstorage "github.com/deckhouse/deckhouse/pkg/metrics-storage"
	"github.com/deckhouse/deckhouse/pkg/metrics-storage/collectors"
	"github.com/deckhouse/deckhouse/pkg/metrics-storage/options"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// vcrObjectsGroup holds every series of the metric, so that an update can drop them all at once.
const vcrObjectsGroup = "vcr_objects"

// vcrObjectsMetric publishes the number of VolumeCaptureRequests in the cache by label set.
type vcrObjectsMetric struct {
	reader client.Reader
	gauge  *collectors.ConstGaugeCollector
}

// vcrLabelSet holds the label values of one series of the metric.
type vcrLabelSet struct {
	mode         string
	driver       string
	storageClass string
	state        string
}

// registerVCRObjects registers the metric in st and returns the function that recounts it.
func registerVCRObjects(st *metricsstorage.MetricStorage, reader client.Reader) (updateFunc, error) {
	gauge, err := st.RegisterGauge(
		"storage_foundation_vcr_objects",
		[]string{labelMode, labelDriver, labelStorageClass, labelState},
		options.WithHelp("VolumeCaptureRequests in the cluster by mode, CSI driver, StorageClass and state."),
	)
	if err != nil {
		return nil, fmt.Errorf("register storage_foundation_vcr_objects: %w", err)
	}

	metric := &vcrObjectsMetric{
		reader: reader,
		gauge:  gauge,
	}

	return metric.update, nil
}

// update replaces the series with the current count, or drops them when the requests cannot be read.
func (m *vcrObjectsMetric) update(ctx context.Context) {
	counts, err := m.countRequests(ctx)

	// Expired before the error check, so a failed read leaves no stale series.
	m.gauge.ExpireGroupMetrics(vcrObjectsGroup)
	if err != nil {
		log.FromContext(ctx).Error(err, "count VolumeCaptureRequests")
		return
	}

	for labelSet, count := range counts {
		labels := map[string]string{
			labelMode:         labelSet.mode,
			labelDriver:       labelSet.driver,
			labelStorageClass: labelSet.storageClass,
			labelState:        labelSet.state,
		}

		m.gauge.Set(float64(count), labels, collectors.WithGroup(vcrObjectsGroup))
	}
}

// countRequests returns the number of requests in the cache by label set. It reads the source PVC
// and PV of each request.
func (m *vcrObjectsMetric) countRequests(ctx context.Context) (map[vcrLabelSet]int, error) {
	requests := new(v1alpha1.VolumeCaptureRequestList)
	if err := m.reader.List(ctx, requests); err != nil {
		return nil, fmt.Errorf("list VolumeCaptureRequests: %w", err)
	}

	counts := make(map[vcrLabelSet]int)

	// No requests, so the StorageClasses are not needed.
	if len(requests.Items) == 0 {
		return counts, nil
	}

	provisioners, err := storageClassProvisioners(ctx, m.reader)
	if err != nil {
		return nil, fmt.Errorf("list StorageClasses: %w", err)
	}

	for i := range requests.Items {
		vcr := &requests.Items[i]
		driver, scName := vcrVolumeLabels(ctx, m.reader, vcr, provisioners)
		labelSet := vcrLabelSet{
			mode:         vcrMode(vcr),
			driver:       driver,
			storageClass: scName,
			state:        vcrState(vcr),
		}

		counts[labelSet]++
	}

	return counts, nil
}
