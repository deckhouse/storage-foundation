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
	"time"

	metricsstorage "github.com/deckhouse/deckhouse/pkg/metrics-storage"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// eventTimeout stops a stuck read while an event is counted.
const eventTimeout = 5 * time.Second

// Recorder writes the metrics that change on an event, such as a created VolumeCaptureRequest.
type Recorder struct {
	reader client.Reader
	vcr    vcrMetrics
}

// newRecorder registers the metrics of the recorder in st and returns the recorder that writes them.
func newRecorder(st *metricsstorage.MetricStorage, reader client.Reader) (*Recorder, error) {
	vcr, err := registerVCRMetrics(st)
	if err != nil {
		return nil, err
	}

	return &Recorder{reader: reader, vcr: vcr}, nil
}
