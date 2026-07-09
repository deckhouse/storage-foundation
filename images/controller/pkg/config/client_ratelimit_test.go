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

package config

import "testing"

func TestResolveClientRateLimit(t *testing.T) {
	const (
		defQPS   float32 = 50
		defBurst int     = 100
	)
	tests := []struct {
		name      string
		qpsRaw    string
		burstRaw  string
		wantQPS   float32
		wantBurst int
		wantErr   bool
	}{
		{name: "both empty -> defaults", wantQPS: defQPS, wantBurst: defBurst},
		{name: "whitespace -> defaults", qpsRaw: "  ", burstRaw: "\t", wantQPS: defQPS, wantBurst: defBurst},
		{name: "override both", qpsRaw: "200", burstRaw: "400", wantQPS: 200, wantBurst: 400},
		{name: "fractional qps", qpsRaw: "12.5", burstRaw: "20", wantQPS: 12.5, wantBurst: 20},
		{name: "invalid qps", qpsRaw: "abc", wantErr: true},
		{name: "zero qps", qpsRaw: "0", wantErr: true},
		{name: "invalid burst", burstRaw: "x", wantErr: true},
		{name: "negative burst", burstRaw: "-5", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			qps, burst, err := resolveClientRateLimit("QPS_ENV", tc.qpsRaw, "BURST_ENV", tc.burstRaw, defQPS, defBurst)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got qps=%v burst=%v", qps, burst)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if qps != tc.wantQPS || burst != tc.wantBurst {
				t.Fatalf("got qps=%v burst=%v, want qps=%v burst=%v", qps, burst, tc.wantQPS, tc.wantBurst)
			}
		})
	}
}
