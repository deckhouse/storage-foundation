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

func TestResolveMaxConcurrentReconciles(t *testing.T) {
	const env = "STORAGE_FOUNDATION_VCR_MAX_CONCURRENT_RECONCILES"

	cases := []struct {
		name    string
		raw     string
		def     int
		want    int
		wantErr bool
	}{
		{name: "unset keeps default", raw: "", def: 4, want: 4},
		{name: "whitespace keeps default", raw: "   ", def: 4, want: 4},
		{name: "override 8", raw: "8", def: 4, want: 8},
		{name: "override 16", raw: "16", def: 4, want: 16},
		{name: "trimmed override", raw: " 12 ", def: 4, want: 12},
		{name: "zero rejected", raw: "0", def: 4, wantErr: true},
		{name: "negative rejected", raw: "-1", def: 4, wantErr: true},
		{name: "non-integer rejected", raw: "abc", def: 4, wantErr: true},
		{name: "float rejected", raw: "4.5", def: 4, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMaxConcurrentReconciles(env, tc.raw, tc.def)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for raw=%q, got value %d", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for raw=%q: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("raw=%q: got %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
