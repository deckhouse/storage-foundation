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

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Controller concurrency override env vars. Unset/empty keeps the built-in default; the value is read
// ONCE at process start (MaxConcurrentReconciles is consumed when the controller is built), so changing
// it requires a pod restart / rollout restart, not a hot reload.
const (
	EnvVCRMaxConcurrentReconciles = "STORAGE_FOUNDATION_VCR_MAX_CONCURRENT_RECONCILES"
)

// ParseMaxConcurrentReconciles reads a controller worker count from the given env var, falling back to
// the supplied default when unset/empty. An invalid (non-integer or non-positive) value returns an error
// so the caller can fail fast rather than silently starting with a wrong worker pool.
func ParseMaxConcurrentReconciles(envName string, def int) (int, error) {
	return resolveMaxConcurrentReconciles(envName, os.Getenv(envName), def)
}

// resolveMaxConcurrentReconciles is the pure parser (no env access) so it can be unit-tested directly.
func resolveMaxConcurrentReconciles(name, raw string, def int) (int, error) {
	n := def
	if v := strings.TrimSpace(raw); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%s=%q: not a valid integer: %w", name, v, err)
		}
		if p <= 0 {
			return 0, fmt.Errorf("%s=%q: must be > 0", name, v)
		}
		n = p
	}
	return n, nil
}
