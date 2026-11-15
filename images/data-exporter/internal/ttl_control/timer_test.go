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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/deckhouse/sds-common-lib/logger"
)

// Every test runs inside a synctest bubble with the production tick (tickerInterval) and the default idle
// window (defaultServerTTL): the clock is virtual, so the real cadence costs no wall-clock time.
const (
	testTTL = defaultServerTTL

	// A long transfer moves transferChunk bytes at a time and pauses transferPause after each of its
	// transferChunks steps: nominally 256s of virtual time, i.e. more than four idle windows.
	transferChunk  = 512
	transferChunks = 128
	transferPause  = 2 * time.Second

	// minTransferSpan is the floor the long-transfer tests assert on their own measured duration. Below it
	// a transfer would be shorter than a single idle window and "the timer did not expire" would hold for
	// the wrong reason — a green run that proves nothing.
	minTransferSpan = 3 * testTTL
)

// startIdleTimer builds an IdleTimer, runs it, and stops it when the test ends. withConsumer keeps a healthy
// consumer of TickerChan in the picture; TestIdleTimer_ExpiresWhenTickConsumerIsStuck deliberately omits it.
// It must be called inside a synctest bubble: the timer's ticker and goroutines belong to it.
func startIdleTimer(t *testing.T, withConsumer bool) *IdleTimer {
	t.Helper()
	tm := NewIdleTimer(testTTL, logger.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go tm.Run(ctx, &wg)
	if withConsumer {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drainTickerChan(ctx, tm)
		}()
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return tm
}

// drainTickerChan reads every tick the timer publishes.
func drainTickerChan(ctx context.Context, tm *IdleTimer) {
	for {
		select {
		case <-tm.TickerChan:
		case <-ctx.Done():
			return
		}
	}
}

// fastPathBody is a request body that offers io.Copy both of its shortcuts: io.WriterTo (taken when the
// body is the source, which is how the upload handlers copy it) and io.ReaderFrom. Real bodies do offer the
// first one — io.NopCloser over a *strings.Reader is one such body — and the counting wrapper has to hide
// both, so the material these tests feed it must carry both. Otherwise the assertions could not tell a
// hidden shortcut from an absent one.
type fastPathBody struct {
	*strings.Reader
}

func (fastPathBody) Close() error { return nil }

// ReadFrom is never called; it exists only so this body advertises io.ReaderFrom.
func (fastPathBody) ReadFrom(io.Reader) (int64, error) { return 0, nil }

// readSlowly consumes src chunk bytes at a time, pausing after every read. It stands for a volume that
// accepts data slower than the client sends it: the transfer then lasts a while without needing
// a large payload.
//
// The running total goes into progress on every read rather than being returned at the end, and that is the
// point: the failure branches of the tests below fire WHILE a transfer is still in the loop, and a total
// published only after the loop would make every one of them report zero bytes moved. On the run these
// tests are designed to fail — the accounting removed — that zero sends the reader hunting a transport
// problem instead of the missing accounting.
func readSlowly(src io.Reader, chunk int, pause time.Duration, progress *atomic.Int64) error {
	buf := make([]byte, chunk)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			total += int64(n)
			progress.Store(total)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		time.Sleep(pause)
	}
}

// writeSlowly is the download-side twin of readSlowly: it writes chunks pieces of chunk bytes to dst,
// pausing after each, and publishes its running total the same way and for the same reason.
func writeSlowly(dst io.Writer, chunk, chunks int, pause time.Duration, progress *atomic.Int64) error {
	buf := bytes.Repeat([]byte("x"), chunk)
	var total int64
	for range chunks {
		n, err := dst.Write(buf)
		total += int64(n)
		progress.Store(total)
		if err != nil {
			return err
		}
		time.Sleep(pause)
	}
	return nil
}

// TestIdleTimer_ExpiresWhenIdle: with no bytes transferred, the idle countdown elapses and ExpiredChan fires.
func TestIdleTimer_ExpiresWhenIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		tm := startIdleTimer(t, true)

		ts := <-tm.ExpiredChan

		require.Greater(t, ts.Sub(started), testTTL, "the timer must not fire before the idle window has elapsed")
		require.LessOrEqual(t, ts.Sub(started), testTTL+tickerInterval, "the timer must fire on the first tick after the idle window")
	})
}

// TestIdleTimer_DoesNotExpireWhileBytesFlow pins the core invariant — never interrupt an active
// transfer — at the level of the counter itself: while bytes keep being reported, each tick resets the
// last-request timestamp, so the idle countdown never completes and ExpiredChan must NOT fire. The two
// tests below do the same through the real middleware, for each direction.
func TestIdleTimer_DoesNotExpireWhileBytesFlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm := startIdleTimer(t, true)

		feed, stopFeed := context.WithCancel(t.Context())
		var feeder sync.WaitGroup
		feeder.Add(1)
		go func() {
			defer feeder.Done()
			ft := time.NewTicker(transferPause)
			defer ft.Stop()
			for {
				select {
				case <-ft.C:
					tm.AddTransferredBytes(1)
				case <-feed.Done():
					return
				}
			}
		}()

		time.Sleep(6 * testTTL)
		synctest.Wait()

		select {
		case <-tm.ExpiredChan:
			t.Fatal("IdleTimer expired while bytes were actively flowing (active transfer must never be interrupted)")
		default:
			t.Logf("no expiry over %s of continuous traffic; %d bytes reported", 6*testTTL, tm.transferredBytes.Load())
		}
		stopFeed()
		feeder.Wait()
	})
}

// TestRequestNotifier_UploadLongerThanTTLDoesNotExpire is the regression test for the upload direction of
// the idle window. An upload handler of this server answers only after the whole body has reached the
// volume, so nothing is written to the response while the body is being consumed: unless the bytes READ
// FROM THE REQUEST BODY count as activity, the window of an upload is measured from the moment the server
// started, and any upload slower than spec.ttl is cut off mid-flight. The upload here lasts several idle
// windows and must survive.
func TestRequestNotifier_UploadLongerThanTTLDoesNotExpire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm := startIdleTimer(t, true)

		var bodyRead atomic.Int64
		handler := RequestNotifier(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := readSlowly(r.Body, transferChunk, transferPause, &bodyRead); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// Deliberately no response body: this is what the upload handlers do.
			w.WriteHeader(http.StatusOK)
		}), tm)

		payload := bytes.Repeat([]byte("x"), transferChunk*transferChunks)

		type uploadResult struct {
			status  int
			elapsed time.Duration
		}
		done := make(chan uploadResult, 1)
		go func() {
			started := time.Now()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload)))
			done <- uploadResult{status: rec.Code, elapsed: time.Since(started)}
		}()

		select {
		case ts := <-tm.ExpiredChan:
			t.Fatalf("idle timer expired at %s while an upload was in flight: the handler had read %d of %d bytes by then, idle window is %s",
				ts.Format(time.RFC3339Nano), bodyRead.Load(), len(payload), testTTL)
		case res := <-done:
			require.Equal(t, http.StatusOK, res.status)
			require.EqualValues(t, len(payload), bodyRead.Load(), "the handler must have consumed the whole body")
			require.Greater(t, res.elapsed, minTransferSpan,
				"the upload finished too fast to prove anything: it must outlast the idle window by a margin")
			t.Logf("upload of %d bytes took %s (%.1f idle windows of %s) without expiring",
				len(payload), res.elapsed, float64(res.elapsed)/float64(testTTL), testTTL)
		}
	})
}

// TestRequestNotifier_DownloadLongerThanTTLDoesNotExpire is a parity pin for the download direction, NOT a
// second regression test: bytes written to the response have always moved the idle window, so this spec is
// green on both sides of the change that added the upload direction. It is here so that a later rework of
// the counting cannot silently break the direction that already worked.
func TestRequestNotifier_DownloadLongerThanTTLDoesNotExpire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm := startIdleTimer(t, true)

		var bodyWritten atomic.Int64
		handler := RequestNotifier(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if err := writeSlowly(w, transferChunk, transferChunks, transferPause, &bodyWritten); err != nil {
				t.Errorf("slow response write failed after %d bytes: %v", bodyWritten.Load(), err)
			}
		}), tm)

		type downloadResult struct {
			received int
			elapsed  time.Duration
		}
		done := make(chan downloadResult, 1)
		go func() {
			started := time.Now()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			done <- downloadResult{received: rec.Body.Len(), elapsed: time.Since(started)}
		}()

		select {
		case ts := <-tm.ExpiredChan:
			t.Fatalf("idle timer expired at %s while a download was in flight: the handler had written %d of %d bytes by then, idle window is %s",
				ts.Format(time.RFC3339Nano), bodyWritten.Load(), transferChunk*transferChunks, testTTL)
		case res := <-done:
			require.EqualValues(t, transferChunk*transferChunks, res.received, "the client must have received the whole response")
			require.Greater(t, res.elapsed, minTransferSpan,
				"the download finished too fast to prove anything: it must outlast the idle window by a margin")
			t.Logf("download of %d bytes took %s (%.1f idle windows of %s) without expiring",
				res.received, res.elapsed, float64(res.elapsed)/float64(testTTL), testTTL)
		}
	})
}

// TestRequestNotifier_CountsTransferredBytesOnce pins that a transferred byte is counted exactly once — no
// direction is missed, and none is counted twice. The timer is not run here: the subject is the counting,
// so the counter is read directly after a single request. The upload half deliberately consumes the body
// with io.Copy, the way the upload handlers do, over a body that offers io.Copy its shortcuts.
func TestRequestNotifier_CountsTransferredBytesOnce(t *testing.T) {
	const payloadSize = 4096

	t.Run("upload counts the bytes read from the request body", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm := NewIdleTimer(testTTL, logger.NewNop())
			defer tm.ticker.Stop()

			handler := RequestNotifier(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			}), tm)

			body := fastPathBody{strings.NewReader(strings.Repeat("x", payloadSize))}
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/", body))

			counted := tm.transferredBytes.Load()
			t.Logf("counted %d bytes for an upload of %d", counted, payloadSize)
			require.EqualValues(t, payloadSize, counted, "every uploaded byte must be counted exactly once")
		})
	})

	t.Run("download counts the bytes written to the response", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm := NewIdleTimer(testTTL, logger.NewNop())
			defer tm.ticker.Stop()

			handler := RequestNotifier(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := w.Write(make([]byte, payloadSize)); err != nil {
					t.Errorf("response write failed: %v", err)
				}
			}), tm)

			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

			counted := tm.transferredBytes.Load()
			t.Logf("counted %d bytes for a download of %d", counted, payloadSize)
			require.EqualValues(t, payloadSize, counted, "every downloaded byte must be counted exactly once")
		})
	})
}

// TestRequestNotifier_BodyWrapperHidesCopyFastPaths pins why the counting body wrapper implements nothing
// but Read and Close. io.Copy prefers io.WriterTo on its source and io.ReaderFrom on its destination over
// the plain read loop, so a body that passes either through is copied to the volume without a single call
// to Read: every uploaded byte goes uncounted while the wrapper still looks correct, and an upload longer
// than spec.ttl dies again. The body fed in here advertises both shortcuts, so the assertions can tell a
// hidden shortcut from an absent one.
func TestRequestNotifier_BodyWrapperHidesCopyFastPaths(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm := NewIdleTimer(testTTL, logger.NewNop())
		defer tm.ticker.Stop()

		var (
			wrapped               bool
			innerHasWriterTo      bool
			innerHasReaderFrom    bool
			exposedWriterTo       bool
			exposedReaderFrom     bool
			exposedFastPathsCount int
		)

		handler := RequestNotifier(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			watcher, ok := r.Body.(*requestBodyWatcher)
			wrapped = ok
			if ok {
				_, innerHasWriterTo = watcher.body.(io.WriterTo)
				_, innerHasReaderFrom = watcher.body.(io.ReaderFrom)
			}
			_, exposedWriterTo = r.Body.(io.WriterTo)
			_, exposedReaderFrom = r.Body.(io.ReaderFrom)
			for _, exposed := range []bool{exposedWriterTo, exposedReaderFrom} {
				if exposed {
					exposedFastPathsCount++
				}
			}
		}), tm)

		body := fastPathBody{strings.NewReader("payload")}
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/", body))

		require.True(t, wrapped, "RequestNotifier must hand the handler the counting body wrapper; without it an upload is not counted at all")
		require.True(t, innerHasWriterTo, "void test material: the body given to the middleware must itself implement io.WriterTo")
		require.True(t, innerHasReaderFrom, "void test material: the body given to the middleware must itself implement io.ReaderFrom")

		t.Logf("checked 2 io.Copy shortcuts on the wrapper (io.WriterTo, io.ReaderFrom); %d exposed", exposedFastPathsCount)
		require.False(t, exposedWriterTo, "the counting body wrapper must not implement io.WriterTo: io.Copy would take it and read the body without counting")
		require.False(t, exposedReaderFrom, "the counting body wrapper must not implement io.ReaderFrom")
	})
}

// TestRequestNotifier_ServesRequestWithoutBody covers the nil-body guard of the middleware. A request that
// came off the wire always has a body (an empty one is http.NoBody), but a hand-built request does not have
// to, and a wrapper put over a nil body would hand the handler a body that panics on its first Read. The
// download direction must still be counted for such a request.
func TestRequestNotifier_ServesRequestWithoutBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm := NewIdleTimer(testTTL, logger.NewNop())
		defer tm.ticker.Stop()

		const payloadSize = 128
		served := false
		handler := RequestNotifier(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served = true
			require.Nil(t, r.Body, "the middleware must not put a counting wrapper over a nil body")
			if _, err := w.Write(make([]byte, payloadSize)); err != nil {
				t.Errorf("response write failed: %v", err)
			}
		}), tm)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Body = nil
		handler.ServeHTTP(httptest.NewRecorder(), req)

		require.True(t, served, "the handler must have been reached")
		counted := tm.transferredBytes.Load()
		t.Logf("counted %d bytes for a bodyless request that wrote %d", counted, payloadSize)
		require.EqualValues(t, payloadSize, counted, "the response of a bodyless request must still be counted")
	})
}

// TestIdleTimer_ExpiresWhenTickConsumerIsStuck: nobody ever reads TickerChan, which is what a consumer stuck
// on a slow or hanging status update looks like. The timer must keep working anyway — ticks are dropped, not
// waited on. If the send blocked instead, the first tick of an active transfer would find the one-slot buffer
// full and stop the whole loop for good: neither the accessTimestamp refresh nor the expiry would ever happen
// again, which is worse than the bug this counting was built for.
func TestIdleTimer_ExpiresWhenTickConsumerIsStuck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm := startIdleTimer(t, false)

		// Report bytes for several ticks, so the loop takes its "transfer in progress" branch — the branch that
		// publishes a tick — while the buffer is already full.
		const activityTicks = 6
		for range activityTicks * int(tickerInterval/transferPause) {
			tm.AddTransferredBytes(1)
			time.Sleep(transferPause)
		}
		t.Logf("reported %d bytes over ~%d ticks with nobody reading TickerChan", tm.transferredBytes.Load(), activityTicks)

		select {
		case <-tm.ExpiredChan:
			// expected: the loop survived the undrained TickerChan and the idle window still elapsed.
		case <-time.After(2 * testTTL):
			t.Fatalf("IdleTimer did not expire within %s after activity stopped: a stuck TickerChan consumer must not stop the timer", 2*testTTL)
		}
	})
}
