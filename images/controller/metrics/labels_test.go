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
	"testing"
	"time"
)

func TestAgeBucket(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"success: a creation time after now is lt_3m", -time.Second, "lt_3m"},
		{"success: zero age is lt_3m", 0, "lt_3m"},
		{"success: just under 3m is lt_3m", 3*time.Minute - time.Nanosecond, "lt_3m"},
		{"success: 3m is 3m_15m", 3 * time.Minute, "3m_15m"},
		{"success: just under 15m is 3m_15m", 15*time.Minute - time.Nanosecond, "3m_15m"},
		{"success: 15m is 15m_30m", 15 * time.Minute, "15m_30m"},
		{"success: just under 30m is 15m_30m", 30*time.Minute - time.Nanosecond, "15m_30m"},
		{"success: 30m is 30m_2h", 30 * time.Minute, "30m_2h"},
		{"success: just under 2h is 30m_2h", 2*time.Hour - time.Nanosecond, "30m_2h"},
		{"success: 2h is 2h_24h", 2 * time.Hour, "2h_24h"},
		{"success: just under 24h is 2h_24h", 24*time.Hour - time.Nanosecond, "2h_24h"},
		{"success: 24h is gt_24h", 24 * time.Hour, "gt_24h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ageBucket(now.Add(-tt.age), now)
			if got != tt.want {
				t.Errorf("ageBucket(now-%v, now) = %q; want %q", tt.age, got, tt.want)
			}
		})
	}
}
