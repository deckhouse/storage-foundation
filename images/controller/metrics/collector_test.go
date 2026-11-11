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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestLeaderCollector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		elected   bool
		wantCalls int32
		want      string
	}{
		{
			name:      "success: a replica that has not been elected neither collects nor publishes",
			wantCalls: 0,
		},
		{
			name:      "success: the elected leader publishes the series of the inner collector",
			elected:   true,
			wantCalls: 1,
			want:      "# HELP test_gauge Test gauge.\n# TYPE test_gauge gauge\ntest_gauge 1\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inner := &stubCollector{desc: prometheus.NewDesc("test_gauge", "Test gauge.", nil, nil)}
			elected := make(chan struct{})
			if tt.elected {
				close(elected)
			}

			if err := testutil.CollectAndCompare(newLeaderCollector(inner, elected), strings.NewReader(tt.want)); err != nil {
				t.Errorf("CollectAndCompare() error = %v", err)
			}
			if got := inner.calls.Load(); got != tt.wantCalls {
				t.Errorf("inner Collect calls = %d; want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestSendGauge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		labelValue string
		want       int
	}{
		{
			name:       "success: a valid label value makes one series",
			labelValue: "a",
			want:       1,
		},
		{
			name:       "error: a label value that is not valid UTF-8 is skipped instead of failing the scrape",
			labelValue: "\xff",
			want:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			desc := prometheus.NewDesc("test_gauge", "Test gauge.", []string{"label"}, nil)
			ch := make(chan prometheus.Metric, 1)

			sendGauge(t.Context(), ch, desc, 1, tt.labelValue)
			close(ch)

			if got := len(ch); got != tt.want {
				t.Errorf("sendGauge(%q) sent %d series; want %d", tt.labelValue, got, tt.want)
			}
		})
	}
}

// stubCollector sends one constant series and counts its Collect calls.
type stubCollector struct {
	desc  *prometheus.Desc
	calls atomic.Int32
}

func (s *stubCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- s.desc
}

func (s *stubCollector) Collect(ch chan<- prometheus.Metric) {
	s.calls.Add(1)
	ch <- prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, 1)
}
