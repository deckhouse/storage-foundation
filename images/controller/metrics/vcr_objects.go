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
	"time"

	metricsstorage "github.com/deckhouse/deckhouse/pkg/metrics-storage"
	"github.com/deckhouse/deckhouse/pkg/metrics-storage/collectors"
	"github.com/deckhouse/deckhouse/pkg/metrics-storage/options"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// vcrObjectsGroup holds every series of the metrics, so that an update can drop them all at once.
const vcrObjectsGroup = "vcr_objects"

// vcrObjectsMetric publishes the number of VolumeCaptureRequests in the cache by label set, and the number of
// pending ones by age and stage.
type vcrObjectsMetric struct {
	reader  client.Reader
	gauge   *collectors.ConstGaugeCollector
	pending *collectors.ConstGaugeCollector
}

// vcrLabelSet holds the label values of one series of storage_foundation_vcr_objects.
type vcrLabelSet struct {
	mode   string
	volume vcrVolume
	state  string
}

// vcrPendingLabelSet holds the label values of one series of storage_foundation_vcr_pending_objects.
type vcrPendingLabelSet struct {
	mode   string
	volume vcrVolume
	age    string
	stage  string
}

// vcrCounts holds the number of requests by label set of each metric.
type vcrCounts struct {
	objects map[vcrLabelSet]int
	pending map[vcrPendingLabelSet]int
}

// registerVCRObjects registers the metrics in st and returns the function that recounts them.
func registerVCRObjects(st *metricsstorage.MetricStorage, reader client.Reader) (updateFunc, error) {
	gauge, err := st.RegisterGauge(
		"storage_foundation_vcr_objects",
		[]string{labelMode, labelDriver, labelStorageClass, labelVolumeSnapshotClass, labelState},
		options.WithHelp("VolumeCaptureRequests in the cluster by mode, CSI driver, StorageClass, "+
			"VolumeSnapshotClass and state."),
	)
	if err != nil {
		return nil, fmt.Errorf("register storage_foundation_vcr_objects: %w", err)
	}

	pending, err := st.RegisterGauge(
		"storage_foundation_vcr_pending_objects",
		[]string{labelMode, labelDriver, labelStorageClass, labelVolumeSnapshotClass, labelAge, labelStage},
		options.WithHelp("Pending VolumeCaptureRequests in the cluster by mode, CSI driver, StorageClass, "+
			"VolumeSnapshotClass, age and stage."),
	)
	if err != nil {
		return nil, fmt.Errorf("register storage_foundation_vcr_pending_objects: %w", err)
	}

	metric := &vcrObjectsMetric{
		reader:  reader,
		gauge:   gauge,
		pending: pending,
	}

	return metric.update, nil
}

// update replaces the series with the current count, or drops them when the requests cannot be read.
func (m *vcrObjectsMetric) update(ctx context.Context) {
	counts, err := m.countRequests(ctx, time.Now())

	// Expired before the error check, so a failed read leaves no stale series.
	m.gauge.ExpireGroupMetrics(vcrObjectsGroup)
	m.pending.ExpireGroupMetrics(vcrObjectsGroup)
	if err != nil {
		log.FromContext(ctx).Error(err, "count VolumeCaptureRequests")
		return
	}

	for labelSet, count := range counts.objects {
		labels := map[string]string{
			labelMode:                labelSet.mode,
			labelDriver:              labelSet.volume.driver,
			labelStorageClass:        labelSet.volume.storageClass,
			labelVolumeSnapshotClass: labelSet.volume.snapshotClass,
			labelState:               labelSet.state,
		}

		m.gauge.Set(float64(count), labels, collectors.WithGroup(vcrObjectsGroup))
	}

	for labelSet, count := range counts.pending {
		labels := map[string]string{
			labelMode:                labelSet.mode,
			labelDriver:              labelSet.volume.driver,
			labelStorageClass:        labelSet.volume.storageClass,
			labelVolumeSnapshotClass: labelSet.volume.snapshotClass,
			labelAge:                 labelSet.age,
			labelStage:               labelSet.stage,
		}

		m.pending.Set(float64(count), labels, collectors.WithGroup(vcrObjectsGroup))
	}
}

// countRequests counts the requests in the cache by label set of each metric, with the age of a pending one
// measured at now. It reads the source PVC and PV of each request.
func (m *vcrObjectsMetric) countRequests(ctx context.Context, now time.Time) (vcrCounts, error) {
	requests := new(v1alpha1.VolumeCaptureRequestList)
	if err := m.reader.List(ctx, requests); err != nil {
		return vcrCounts{}, fmt.Errorf("list VolumeCaptureRequests: %w", err)
	}

	counts := vcrCounts{
		objects: make(map[vcrLabelSet]int),
		pending: make(map[vcrPendingLabelSet]int),
	}

	// No requests, so the StorageClasses are not needed.
	if len(requests.Items) == 0 {
		return counts, nil
	}

	classes, err := storageClasses(ctx, m.reader)
	if err != nil {
		return vcrCounts{}, fmt.Errorf("list StorageClasses: %w", err)
	}

	for i := range requests.Items {
		vcr := &requests.Items[i]
		mode := vcrMode(vcr)
		volume := vcrVolumeLabels(ctx, m.reader, vcr, classes)
		state := vcrState(vcr)

		counts.objects[vcrLabelSet{mode: mode, volume: volume, state: state}]++

		if state != statePending {
			continue
		}

		pendingLabelSet := vcrPendingLabelSet{
			mode:   mode,
			volume: volume,
			age:    ageBucket(vcr.CreationTimestamp.Time, now),
			stage:  vcrStage(vcr),
		}

		counts.pending[pendingLabelSet]++
	}

	return counts, nil
}
