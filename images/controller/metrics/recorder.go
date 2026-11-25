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
	"maps"
	"time"

	metricsstorage "github.com/deckhouse/deckhouse/pkg/metrics-storage"
	"github.com/deckhouse/deckhouse/pkg/metrics-storage/collectors"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// eventTimeout stops a stuck read while an event is counted.
const eventTimeout = 5 * time.Second

// Recorder writes the metrics that change on an event, such as a created VolumeCaptureRequest.
type Recorder struct {
	reader client.Reader
	vcr    vcrEventMetrics
	// byNamespace lists the collectors written through incInNamespace and observeInNamespace.
	// NamespaceEvents expires their series, so a collector written that way must be listed here.
	byNamespace []collectors.ConstCollector
}

// newRecorder registers the metrics of the recorder in st and returns the recorder that writes them.
func newRecorder(st *metricsstorage.MetricStorage, reader client.Reader) (*Recorder, error) {
	vcr, err := registerVCREventMetrics(st)
	if err != nil {
		return nil, err
	}

	return &Recorder{
		reader:      reader,
		vcr:         vcr,
		byNamespace: []collectors.ConstCollector{vcr.created, vcr.duration},
	}, nil
}

// NamespaceEvents returns the event handler that drops the series written through incInNamespace and
// observeInNamespace for a deleted namespace. It enqueues nothing.
func (r *Recorder) NamespaceEvents() handler.Funcs {
	return handler.Funcs{
		DeleteFunc: func(_ context.Context, e event.DeleteEvent, _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			for _, c := range r.byNamespace {
				c.ExpireGroupMetrics(e.Object.GetName())
			}
		},
	}
}

// incInNamespace adds one to the series of c that has labels and the namespace of obj.
// The series is in the group named after that namespace, which NamespaceEvents expires.
func incInNamespace(c *collectors.ConstCounterCollector, obj client.Object, labels map[string]string) {
	c.Add(1, withNamespace(obj, labels), collectors.WithGroup(obj.GetNamespace()))
}

// observeInNamespace records value in the series of h that has labels and the namespace of obj.
// The series is grouped as in incInNamespace.
func observeInNamespace(
	h *collectors.ConstHistogramCollector,
	obj client.Object,
	value float64,
	labels map[string]string,
) {
	h.Observe(value, withNamespace(obj, labels), collectors.WithGroup(obj.GetNamespace()))
}

// withNamespace returns a copy of labels with the namespace label set to the namespace of obj.
func withNamespace(obj client.Object, labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels)+1)
	maps.Copy(result, labels)
	result[labelNamespace] = obj.GetNamespace()

	return result
}
