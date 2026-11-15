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

package ttl_control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/storage-foundation/common"
)

// TestNewTTLControl_TTLParsing covers the --ttl (spec.ttl) parsing, including the empty-ttl fallback: the
// CRD pattern for spec.ttl also matches the empty string, so a producer that sets ttl:"" would otherwise
// crash the server pod on ParseDuration(""). The fallback keeps the pod serving with the documented
// default idle window instead of crash-looping.
func TestNewTTLControl_TTLParsing(t *testing.T) {
	logger := logger.NewNop()

	tests := []struct {
		name    string
		ttlStr  string
		wantTTL time.Duration
		wantErr bool
	}{
		{name: "explicit duration", ttlStr: "30m", wantTTL: 30 * time.Minute},
		{name: "empty falls back to default", ttlStr: "", wantTTL: defaultServerTTL},
		{name: "whitespace-only falls back to default", ttlStr: "   ", wantTTL: defaultServerTTL},
		{name: "invalid is still rejected", ttlStr: "not-a-duration", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := NewTTLControl(common.OperationImport, tt.ttlStr, "ns", "name", nil, nil, logger)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, tc)
			assert.Equal(t, tt.wantTTL, tc.ttl)
		})
	}
}

type fakeTimestampUpdater struct {
	mu    sync.Mutex
	start time.Time
	calls []time.Duration
}

func (f *fakeTimestampUpdater) UpdateAccessTimestamp(_ context.Context, _ common.Operation, _, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, time.Since(f.start))
	return nil
}

func (f *fakeTimestampUpdater) offsets() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.calls...)
}

type fakeServerStateSetter struct {
	mu       sync.Mutex
	start    time.Time
	failures int
	calls    []time.Duration
	states   []common.ServerState
}

func (f *fakeServerStateSetter) SetServerState(_ context.Context, _ common.Operation, _, _ string, state common.ServerState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, time.Since(f.start))
	f.states = append(f.states, state)
	if len(f.calls) <= f.failures {
		return errors.New("status update failed")
	}
	return nil
}

func (f *fakeServerStateSetter) offsets() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.calls...)
}

func startTTLControl(t *testing.T, updater *fakeTimestampUpdater, setter *fakeServerStateSetter) *TTLControl {
	t.Helper()
	start := time.Now()
	updater.start = start
	setter.start = start

	tc, err := NewTTLControl(common.OperationExport, "1m", "ns", "name", updater, setter, logger.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	tc.Start(ctx, &wg)
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return tc
}

func TestTTLControl_TrafficKeepsServerAliveThenIdleExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		updater := &fakeTimestampUpdater{}
		setter := &fakeServerStateSetter{}
		tc := startTTLControl(t, updater, setter)

		handler := tc.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("payload"))
		}))

		time.Sleep(time.Second)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

		time.Sleep(tickerInterval)
		synctest.Wait()
		require.Equal(t, []time.Duration{0, tickerInterval}, updater.offsets(),
			"the timestamp is published at start and again on the first tick that saw traffic")
		require.Empty(t, setter.offsets())

		time.Sleep(2 * tickerInterval)
		synctest.Wait()
		require.Empty(t, setter.offsets(), "idle for no more than the window must not expire the server")

		time.Sleep(2 * tickerInterval)
		synctest.Wait()
		require.Equal(t, []time.Duration{4 * tickerInterval}, setter.offsets(),
			"the server expires on the first tick after a full idle window since the last traffic tick")
		require.Equal(t, []common.ServerState{common.ServerStateIdleExpired}, setter.states)
	})
}

func TestTTLControl_IdleExpiryRetriesUntilStatusUpdateSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		updater := &fakeTimestampUpdater{}
		setter := &fakeServerStateSetter{failures: 2}
		startTTLControl(t, updater, setter)

		time.Sleep(10 * time.Minute)
		synctest.Wait()

		require.Equal(t,
			[]time.Duration{3 * tickerInterval, 3*tickerInterval + setServerStateIdleExpiredRetryPeriod, 3*tickerInterval + 2*setServerStateIdleExpiredRetryPeriod},
			setter.offsets(),
			"a failed IdleExpired update is retried every retry period until it succeeds, and then stops")
	})
}
