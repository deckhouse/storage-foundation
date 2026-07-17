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

package gc

import (
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BaseGcSettings is the per-resource garbage-collection configuration: the retention TTL (measured from
// the object's terminal time) and the cron schedule of the collection sweep. Configuration is env-only
// (no module settings, no per-object annotations).
type BaseGcSettings struct {
	TTL      metav1.Duration
	Schedule string
}

// GetBaseGcSettingsFromEnv overrides def with the TTL/schedule env variables when they are set. A
// non-positive TTL is rejected: 0 would reap an object the instant it becomes terminal, and a negative
// value would make every terminal object (including one with no completionTimestamp yet, age 0) satisfy
// age > ttl and be reaped immediately — silently defeating the zero-time fail-safe in DefaultFilter.
// Ported from the virtualization module's config.GetBaseGCSettingsFromEnv, with the default supplied by
// the caller (each resource ships its own default, e.g. 24h / hourly) instead of a package-level constant.
func GetBaseGcSettingsFromEnv(envSchedule, envTTL string, def BaseGcSettings) (BaseGcSettings, error) {
	settings := def
	if v, ok := os.LookupEnv(envSchedule); ok && v != "" {
		settings.Schedule = v
	}
	if v, ok := os.LookupEnv(envTTL); ok && v != "" {
		t, err := time.ParseDuration(v)
		if err != nil {
			return BaseGcSettings{}, fmt.Errorf("invalid GC TTL %q from %s: %w", v, envTTL, err)
		}
		if t <= 0 {
			return BaseGcSettings{}, fmt.Errorf("invalid GC settings: TTL from %s must be positive, got %s", envTTL, t)
		}
		settings.TTL = metav1.Duration{Duration: t}
	}
	return settings, nil
}
