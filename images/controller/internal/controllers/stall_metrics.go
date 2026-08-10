/*
Copyright 2025 Flant JSC

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

package controllers

import (
	"context"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

const (
	stalledMetricName = "storage_foundation_volume_capture_stalled"
	// unknownDriver is reported when the content behind a stalled request cannot be read. The
	// request is stalled either way, so it must still be counted — just without attribution.
	unknownDriver = "unknown"
	// stallCollectTimeout bounds a scrape. Reads are served from the informer cache, so this only
	// guards against a pathologically slow or shutting-down cache.
	stallCollectTimeout = 5 * time.Second
)

// stallCollector reports how many capture requests are currently stalled, by diagnosis and CSI
// driver.
//
// It is a collector that recomputes from the informer cache on every scrape, deliberately not a
// mutable GaugeVec written from reconcile. A gauge carrying a reason label leaks by construction:
// the series {reason="SnapshotStackUnavailable"} would survive the request leaving the stall,
// switching to another reason, and being deleted, leaving a permanent false 1 in monitoring. Here
// the metric state is derived from cluster state, so a series simply stops being produced.
//
// Labels are limited to reason and driver, both bounded. Request and content names stay out of
// labels — they are unbounded, and their place is the condition message and the event.
type stallCollector struct {
	reader client.Reader
	desc   *prometheus.Desc
}

func newStallCollector(reader client.Reader) *stallCollector {
	return &stallCollector{
		reader: reader,
		desc: prometheus.NewDesc(
			stalledMetricName,
			"Number of VolumeCaptureRequests whose snapshot execution shows no observable progress.",
			[]string{"reason", "driver"},
			nil,
		),
	}
}

func (c *stallCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *stallCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), stallCollectTimeout)
	defer cancel()

	list := &storagev1alpha1.VolumeCaptureRequestList{}
	if err := c.reader.List(ctx, list); err != nil {
		// Reporting nothing is better than reporting a stale or partial count: a scrape gap is
		// visible in monitoring, a silently wrong number is not.
		return
	}

	type stallKey struct{ reason, driver string }
	counts := make(map[stallKey]int)
	for i := range list.Items {
		vcr := &list.Items[i]
		cond := meta.FindStatusCondition(vcr.Status.Conditions, storagev1alpha1.ConditionTypeStalled)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			continue
		}
		counts[stallKey{reason: cond.Reason, driver: c.driverFor(ctx, vcr)}]++
	}

	for key, count := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count), key.reason, key.driver)
	}
}

// driverFor resolves the CSI driver of the content the request is waiting for. The whole point of
// the driver label is to tell "this one driver is stuck" from "the snapshot stack is down", so an
// unresolvable driver is reported as such instead of dropping the sample.
func (c *stallCollector) driverFor(ctx context.Context, vcr *storagev1alpha1.VolumeCaptureRequest) string {
	if vcr.Spec.Target == nil || vcr.Spec.Target.UID == "" || vcr.UID == "" {
		return unknownDriver
	}
	vsc := &snapshotv1.VolumeSnapshotContent{}
	if err := c.reader.Get(ctx, client.ObjectKey{Name: snapshotVSCName(vcr.UID, vcr.Spec.Target.UID)}, vsc); err != nil {
		return unknownDriver
	}
	if vsc.Spec.Driver == "" {
		return unknownDriver
	}
	return vsc.Spec.Driver
}
