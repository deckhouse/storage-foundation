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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	envSchedule = "TEST_GC_SCHEDULE"
	envTTL      = "TEST_GC_TTL"
)

func def() BaseGcSettings {
	return BaseGcSettings{TTL: metav1.Duration{Duration: 24 * time.Hour}, Schedule: "0 * * * *"}
}

func TestGetBaseGcSettingsFromEnv_DefaultsWhenUnset(t *testing.T) {
	// Ensure the vars are unset for this test.
	t.Setenv(envSchedule, "")
	t.Setenv(envTTL, "")

	got, err := GetBaseGcSettingsFromEnv(envSchedule, envTTL, def())
	require.NoError(t, err)
	assert.Equal(t, def(), got, "empty env values must be ignored and the defaults kept")
}

func TestGetBaseGcSettingsFromEnv_Overrides(t *testing.T) {
	t.Setenv(envSchedule, "*/5 * * * *")
	t.Setenv(envTTL, "2h")

	got, err := GetBaseGcSettingsFromEnv(envSchedule, envTTL, def())
	require.NoError(t, err)
	assert.Equal(t, "*/5 * * * *", got.Schedule)
	assert.Equal(t, 2*time.Hour, got.TTL.Duration)
}

func TestGetBaseGcSettingsFromEnv_InvalidTTLString(t *testing.T) {
	t.Setenv(envTTL, "not-a-duration")

	_, err := GetBaseGcSettingsFromEnv(envSchedule, envTTL, def())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid GC TTL")
}

// TestGetBaseGcSettingsFromEnv_NonPositiveTTL guards the fail-safe: TTL=0 would reap a just-terminal
// object instantly, and a negative TTL would make even a completionTimestamp-less object (age 0) satisfy
// age > ttl and be reaped — defeating the zero-time fail-safe in DefaultFilter. Both must be rejected.
func TestGetBaseGcSettingsFromEnv_NonPositiveTTL(t *testing.T) {
	for _, v := range []string{"0", "0s", "-1h"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(envTTL, v)
			_, err := GetBaseGcSettingsFromEnv(envSchedule, envTTL, def())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be positive")
		})
	}
}
