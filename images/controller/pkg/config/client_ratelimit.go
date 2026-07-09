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

// Kubernetes client rate-limit override env vars. Unset/empty keeps the built-in defaults; values are read
// ONCE at process start (rest.Config.QPS/Burst are consumed when clients are constructed), so changing them
// requires a pod restart / rollout restart, not a hot reload.
const (
	EnvKubeQPS   = "STORAGE_FOUNDATION_KUBE_QPS"
	EnvKubeBurst = "STORAGE_FOUNDATION_KUBE_BURST"
)

// ParseClientRateLimit reads a QPS/Burst pair from the given env vars, falling back to the supplied defaults
// when unset/empty. An invalid (non-numeric or non-positive) value returns an error so the caller can fail
// fast rather than silently running on the client-go default (QPS 5 / Burst 10). QPS is float32, Burst is int.
func ParseClientRateLimit(qpsEnv, burstEnv string, defQPS float32, defBurst int) (qps float32, burst int, err error) {
	return resolveClientRateLimit(qpsEnv, os.Getenv(qpsEnv), burstEnv, os.Getenv(burstEnv), defQPS, defBurst)
}

// resolveClientRateLimit is the pure parser (no env access) so it can be unit-tested directly.
func resolveClientRateLimit(qpsName, qpsRaw, burstName, burstRaw string, defQPS float32, defBurst int) (float32, int, error) {
	qps, burst := defQPS, defBurst
	if v := strings.TrimSpace(qpsRaw); v != "" {
		f, perr := strconv.ParseFloat(v, 32)
		if perr != nil {
			return 0, 0, fmt.Errorf("%s=%q: not a valid number: %w", qpsName, v, perr)
		}
		if f <= 0 {
			return 0, 0, fmt.Errorf("%s=%q: must be > 0", qpsName, v)
		}
		qps = float32(f)
	}
	if v := strings.TrimSpace(burstRaw); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil {
			return 0, 0, fmt.Errorf("%s=%q: not a valid integer: %w", burstName, v, perr)
		}
		if n <= 0 {
			return 0, 0, fmt.Errorf("%s=%q: must be > 0", burstName, v)
		}
		burst = n
	}
	return qps, burst, nil
}
