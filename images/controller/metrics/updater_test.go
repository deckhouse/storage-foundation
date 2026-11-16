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
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestUpdater_Start(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		elapsed   time.Duration
		wantCalls int32
	}{
		{
			name:      "success: the first update runs at once",
			wantCalls: 1,
		},
		{
			name:      "success: no second update before the period ends",
			elapsed:   updatePeriod - time.Second,
			wantCalls: 1,
		},
		{
			name:      "success: an update runs every period",
			elapsed:   2*updatePeriod + time.Second,
			wantCalls: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				startUpdater(t, func(context.Context) { calls.Add(1) })

				time.Sleep(tt.elapsed)
				synctest.Wait()

				if got := calls.Load(); got != tt.wantCalls {
					t.Errorf("updates after %v = %d; want %d", tt.elapsed, got, tt.wantCalls)
				}
			})
		})
	}
}

func TestUpdater_Start_isolatesUpdates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		first updateFunc
		// wantDeadline is the deadline of the second update, counted from the start.
		wantDeadline time.Duration
	}{
		{
			name:         "success: a fast first update leaves the second its full timeout",
			first:        func(context.Context) {},
			wantDeadline: updateTimeout,
		},
		{
			name:         "success: a slow first update does not shorten the timeout of the second",
			first:        func(context.Context) { time.Sleep(time.Second) },
			wantDeadline: time.Second + updateTimeout,
		},
		{
			name:         "success: a stuck first update is stopped by its own timeout",
			first:        func(ctx context.Context) { <-ctx.Done() },
			wantDeadline: 2 * updateTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				deadlines := make(chan time.Time, 1)
				startUpdater(t, tt.first, func(ctx context.Context) {
					deadline, _ := ctx.Deadline()
					deadlines <- deadline
				})

				// Long enough for the second update to run after a stuck first one, too short for the next tick.
				time.Sleep(updateTimeout + time.Second)
				synctest.Wait()

				select {
				case got := <-deadlines:
					if want := start.Add(tt.wantDeadline); !got.Equal(want) {
						t.Errorf("second update deadline = %v; want %v", got.Sub(start), tt.wantDeadline)
					}
				default:
					t.Errorf("second update did not run")
				}
			})
		})
	}
}

// startUpdater runs an updater of updateFuncs until the test ends, and then checks that Start returns nil.
func startUpdater(t *testing.T, updateFuncs ...updateFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- newUpdater(updateFuncs).Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start() error = %v; want nil", err)
		}
	})
}
