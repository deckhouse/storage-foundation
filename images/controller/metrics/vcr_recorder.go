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

// vcrMetrics holds the metrics of VolumeCaptureRequests that the recorder writes.
type vcrMetrics struct {
	created *collectors.ConstCounterCollector
}

// registerVCRMetrics registers the metrics of VolumeCaptureRequests in st.
func registerVCRMetrics(st *metricsstorage.MetricStorage) (vcrMetrics, error) {
	created, err := st.RegisterCounter(
		"storage_foundation_vcr_created_total",
		[]string{labelMode, labelDriver, labelStorageClass},
		options.WithHelp("VolumeCaptureRequests created in the cluster by mode, CSI driver and StorageClass."),
	)
	if err != nil {
		return vcrMetrics{}, fmt.Errorf("register storage_foundation_vcr_created_total: %w", err)
	}

	return vcrMetrics{created: created}, nil
}

// VCREvents returns the event handler that counts the created VolumeCaptureRequests. It enqueues nothing.
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

			r.countVCRCreated(ctx, vcr)
		},
	}
}

// countVCRCreated counts vcr as a created request. It reads the StorageClasses, the source PVC and the PV.
func (r *Recorder) countVCRCreated(ctx context.Context, vcr *v1alpha1.VolumeCaptureRequest) {
	provisioners, err := storageClassProvisioners(ctx, r.reader)
	if err != nil {
		// The event does not come again, so the request is counted with an unknown StorageClass.
		log.FromContext(ctx).Error(err, "list StorageClasses", "namespace", vcr.Namespace, "name", vcr.Name)
	}

	driver, scName := vcrVolumeLabels(ctx, r.reader, vcr, provisioners)
	r.vcr.created.Add(1, map[string]string{
		labelMode:         vcrMode(vcr),
		labelDriver:       driver,
		labelStorageClass: scName,
	})
}
