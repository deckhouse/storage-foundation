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
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// vcrEventMetrics holds the metrics of VolumeCaptureRequest events that the recorder writes.
type vcrEventMetrics struct {
	created  *collectors.ConstCounterCollector
	duration *collectors.ConstHistogramCollector
}

// registerVCREventMetrics registers the metrics of VolumeCaptureRequest events in st.
func registerVCREventMetrics(st *metricsstorage.MetricStorage) (vcrEventMetrics, error) {
	created, err := st.RegisterCounter(
		"storage_foundation_vcr_created_total",
		[]string{labelNamespace, labelMode, labelDriver, labelStorageClass, labelVolumeSnapshotClass},
		options.WithHelp("VolumeCaptureRequests created in the cluster by namespace, mode, CSI driver, StorageClass "+
			"and VolumeSnapshotClass."),
	)
	if err != nil {
		return vcrEventMetrics{}, fmt.Errorf("register storage_foundation_vcr_created_total: %w", err)
	}

	duration, err := st.RegisterHistogram(
		"storage_foundation_vcr_duration_seconds",
		[]string{labelNamespace, labelMode, labelDriver, labelStorageClass, labelVolumeSnapshotClass, labelResult},
		durationBuckets(),
		options.WithHelp("Time from the creation of a VolumeCaptureRequest to its final status "+
			"by namespace, mode, CSI driver, StorageClass, VolumeSnapshotClass and result."),
	)
	if err != nil {
		return vcrEventMetrics{}, fmt.Errorf("register storage_foundation_vcr_duration_seconds: %w", err)
	}

	return vcrEventMetrics{created: created, duration: duration}, nil
}

// VCREvents returns the event handler that writes the metrics of created and finished VolumeCaptureRequests.
// It enqueues nothing.
func (r *Recorder) VCREvents() handler.Funcs {
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			vcr, ok := e.Object.(*v1alpha1.VolumeCaptureRequest)
			// The initial list holds the requests that existed before this controller started.
			if !ok || e.IsInInitialList {
				return
			}

			ctx, cancel := context.WithTimeout(ctx, eventTimeout)
			defer cancel()

			r.countVCRCreated(vcr, r.resolveVCRVolume(ctx, vcr))
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			oldVCR, ok := e.ObjectOld.(*v1alpha1.VolumeCaptureRequest)
			if !ok {
				return
			}
			vcr, ok := e.ObjectNew.(*v1alpha1.VolumeCaptureRequest)
			if !ok {
				return
			}
			// Only the update that brings the final status: a resync repeats a status that is already final.
			if vcrState(oldVCR) != statePending || vcrState(vcr) == statePending {
				return
			}

			ctx, cancel := context.WithTimeout(ctx, eventTimeout)
			defer cancel()

			r.observeVCRDuration(vcr, r.resolveVCRVolume(ctx, vcr))
		},
	}
}

// resolveVCRVolume returns the volume labels of vcr. It reads the StorageClasses, the source PVC and the PV.
func (r *Recorder) resolveVCRVolume(ctx context.Context, vcr *v1alpha1.VolumeCaptureRequest) vcrVolume {
	classes, err := storageClasses(ctx, r.reader)
	if err != nil {
		// The event does not come again, so the request is labelled as if its StorageClass were unknown.
		log.FromContext(ctx).Error(err, "list StorageClasses", "namespace", vcr.Namespace, "name", vcr.Name)
	}

	return vcrVolumeLabels(ctx, r.reader, vcr, classes)
}

// countVCRCreated counts vcr as a created request.
func (r *Recorder) countVCRCreated(vcr *v1alpha1.VolumeCaptureRequest, volume vcrVolume) {
	incInNamespace(r.vcr.created, vcr, map[string]string{
		labelMode:                vcrMode(vcr),
		labelDriver:              volume.driver,
		labelStorageClass:        volume.storageClass,
		labelVolumeSnapshotClass: volume.snapshotClass,
	})
}

// observeVCRDuration records the time from the creation of the finished vcr to its final status.
func (r *Recorder) observeVCRDuration(vcr *v1alpha1.VolumeCaptureRequest, volume vcrVolume) {
	seconds := vcr.Status.CompletionTimestamp.Sub(vcr.CreationTimestamp.Time).Seconds()
	// The API server stamps the creation and the controller the completion, each by its own clock and to the second,
	// so a request that fails at once may come out negative.
	if seconds < 0 {
		seconds = 0
	}

	observeInNamespace(r.vcr.duration, vcr, seconds, map[string]string{
		labelMode:                vcrMode(vcr),
		labelDriver:              volume.driver,
		labelStorageClass:        volume.storageClass,
		labelVolumeSnapshotClass: volume.snapshotClass,
		labelResult:              vcrState(vcr),
	})
}

// durationBuckets returns the upper bounds in seconds of the duration histogram buckets, from a request that fails
// at once to a snapshot of a slow driver.
func durationBuckets() []float64 {
	return []float64{1, 2.5, 5, 10, 15, 20, 30, 45, 60, 120, 180, 240, 300, 600, 1800, 3600, 7200, 14400, 43200}
}
