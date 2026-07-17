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
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fastIdleTimer builds an IdleTimer with a short TTL and swaps the (unexported) production 30s ticker for a
// fast one so the idle countdown can be exercised in milliseconds. This changes ONLY the tick cadence in
// the test — the production semantics (countdown after last-request, reset on bytes) are untouched.
func fastIdleTimer(ttl, tick time.Duration) *IdleTimer {
	tm := NewIdleTimer(ttl, slog.Default())
	tm.ticker.Stop()
	tm.ticker = time.NewTicker(tick)
	return &tm
}

// drainTickerChan keeps IdleTimer.Run from blocking on its buffered (size 1) TickerChan sends.
func drainTickerChan(ctx context.Context, tm *IdleTimer) {
	for {
		select {
		case <-tm.TickerChan:
		case <-ctx.Done():
			return
		}
	}
}

// TestIdleTimer_ExpiresWhenIdle: with no bytes written, the idle countdown elapses and ExpiredChan fires.
func TestIdleTimer_ExpiresWhenIdle(t *testing.T) {
	tm := fastIdleTimer(50*time.Millisecond, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go tm.Run(ctx, &wg)
	go drainTickerChan(ctx, tm)

	select {
	case <-tm.ExpiredChan:
		// expected: the idle window elapsed with no traffic.
	case <-time.After(3 * time.Second):
		t.Fatal("IdleTimer did not fire ExpiredChan for an idle server")
	}
	cancel()
	wg.Wait()
}

// TestIdleTimer_DoesNotExpireWhileBytesFlow is the core of the plan's decision #1 (never interrupt an
// active transfer): while bytes keep being written, each tick resets the last-request timestamp, so the
// idle countdown never completes and ExpiredChan must NOT fire.
func TestIdleTimer_DoesNotExpireWhileBytesFlow(t *testing.T) {
	tm := fastIdleTimer(50*time.Millisecond, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go tm.Run(ctx, &wg)
	go drainTickerChan(ctx, tm)

	// Feed bytes continuously (every 3ms, well under the 10ms tick) for ~6x the TTL.
	feed, stopFeed := context.WithCancel(ctx)
	go func() {
		ft := time.NewTicker(3 * time.Millisecond)
		defer ft.Stop()
		for {
			select {
			case <-ft.C:
				select {
				case tm.RequestNotifierChan <- 1:
				case <-feed.Done():
					return
				}
			case <-feed.Done():
				return
			}
		}
	}()

	select {
	case <-tm.ExpiredChan:
		t.Fatal("IdleTimer expired while bytes were actively flowing (active transfer must never be interrupted)")
	case <-time.After(300 * time.Millisecond):
		// expected: continuous traffic kept the countdown reset.
	}
	stopFeed()
	cancel()
	wg.Wait()
}
