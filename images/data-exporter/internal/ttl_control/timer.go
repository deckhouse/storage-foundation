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
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Time period for wake up time-to-live timer and update DataExport/DataImport.Status.AccessTimestamp
const tickerInterval = 30 * time.Second

// TransferCounter receives the number of bytes that moved through the data server. It is all RequestNotifier
// needs from the idle timer, and its method must never block: it is called from inside the copy loop of
// every transfer, so a report that waits would stall the transfer it is reporting about.
type TransferCounter interface {
	AddTransferredBytes(n int)
}

// Middleware used for time-to-live control:
// Counts the bytes that move through a request in BOTH directions — those written to http.ResponseWriter
// (download) and those read from the request body (upload) — and reports them to the time-to-live timer
// If all connections finished or freezed (zero bytes moved in either direction in tickerInterval), the
// time-to-live countdown begins; a transfer that lasts longer than the whole time-to-live therefore keeps
// its window open as long as its bytes keep moving
// Each tickerInterval DataExport/DataImport.Status.AccessTimestamp fileld will be updated
// After time-to-live period will expired, status.serverState will be set to IdleExpired
//
// The upload direction is counted here and not in the upload handlers on purpose: one place covers every
// handler that reads a request body, and the handlers stay unaware of the timer.
//
// Know where this middleware sits before reasoning about what counts as activity: it is nested immediately
// INSIDE authorization, and everything else — the required-header check, the router, every handler — is
// inside it. That placement is the definition of activity this server uses: activity is what a caller the
// server has recognised makes the server do. A request refused for missing or rejected credentials never
// reaches this middleware at all, so neither its (unread) body nor the bytes of its refusal move the idle
// window; without that, one anonymous request per idle period would keep the pod, its volume and the
// transfer window alive indefinitely, from anywhere that can reach the published endpoint. Past
// authorization the accounting is unconditional: a refusal a handler writes to a recognised caller — a
// malformed upload, a wrong method, a failed completion the client is about to retry — is counted like any
// other response bytes, because closing the window under a caller who is still working is the one thing
// this window must not do.
func RequestNotifier(next http.Handler, counter TransferCounter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// prohibit page cashing for update idle timer each user click
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		watcher := newResponseWatcher(w, counter)

		if r.Body == nil {
			next.ServeHTTP(&watcher, r)
			return
		}

		// Shallow copy rather than an assignment to r.Body: the request belongs to whoever called this
		// handler, and replacing a field of somebody else's request is not ours to do.
		counted := *r
		counted.Body = &requestBodyWatcher{body: r.Body, counter: counter}
		next.ServeHTTP(&watcher, &counted)
	})
}

// Override method "write" in http.ResponseWriter for report written bytes number to time-to-live timer
type responseWatcher struct {
	http.ResponseWriter
	counter TransferCounter
}

func newResponseWatcher(w http.ResponseWriter, counter TransferCounter) responseWatcher {
	return responseWatcher{
		w,
		counter,
	}
}

func (rw *responseWatcher) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	if err == nil {
		rw.counter.AddTransferredBytes(n)
	}
	return n, err
}

// requestBodyWatcher reports the bytes read from a request body, which is the upload direction of the idle
// window. Without it an upload has no way to move the window at all: an upload handler answers only after
// the whole body has been consumed, so until then nothing is written to the response and the window would
// be counted from the moment the server started rather than from the last activity.
//
// It implements ONLY Read and Close, and must keep it that way — neither io.WriterTo nor io.ReaderFrom.
// io.Copy takes a shortcut over its plain read loop whenever the source offers WriteTo or the destination
// offers ReadFrom, and an upload handler copies the body with io.Copy. A WriteTo method here is therefore
// the one that bites: the body would be drained without a single call to Read, every uploaded byte would go
// uncounted, and this wrapper would still look correct. ReadFrom is excluded together with it so that
// nobody has to re-derive which shortcut belongs to which side before adding a method. For the same reason
// the body is held in a named field instead of being embedded: the method set of this type stays visibly
// exactly Read and Close.
type requestBodyWatcher struct {
	body    io.ReadCloser
	counter TransferCounter
}

func (b *requestBodyWatcher) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	// Report on n > 0 whatever err is: the last Read of a body returns its final bytes together with
	// io.EOF, and those bytes are transfer activity like any other.
	if n > 0 {
		b.counter.AddTransferredBytes(n)
	}
	return n, err
}

func (b *requestBodyWatcher) Close() error {
	return b.body.Close()
}

type IdleTimer struct {
	// transferredBytes is the running total of the bytes that moved through the server in either
	// direction. Every request goroutine adds to it; Run reads it once per tick.
	//
	// It is a counter and not a channel on purpose. A channel send would sit in the hot path of every
	// buffer of every transfer — hundreds of thousands of sends on a multi-gigabyte upload — and it would
	// block as soon as the receiving loop stopped draining it, which is exactly what happens while that
	// loop waits on a send of its own. The transfer would then be stalled by the mechanism whose whole
	// job is to keep it alive.
	transferredBytes atomic.Int64

	TickerChan  chan time.Time
	ExpiredChan chan time.Time

	timeToLive                   time.Duration
	previousTickTransferredBytes int64
	lastRequestTimestamp         time.Time
	ticker                       *time.Ticker
	expired                      bool
	logger                       *slog.Logger
}

// NewIdleTimer returns a pointer and not a value: IdleTimer holds an atomic counter, so it must not be
// copied after construction.
func NewIdleTimer(timeToLive time.Duration, logger *slog.Logger) *IdleTimer {
	return &IdleTimer{
		TickerChan:                   make(chan time.Time, 1),
		ExpiredChan:                  make(chan time.Time, 1),
		timeToLive:                   timeToLive,
		previousTickTransferredBytes: 0,
		lastRequestTimestamp:         time.Now(),
		ticker:                       time.NewTicker(tickerInterval),
		expired:                      false,
		logger:                       logger,
	}
}

// AddTransferredBytes reports n bytes that moved through the server in either direction. It never blocks
// and never waits on Run: it is called from inside the copy loop of every transfer.
func (t *IdleTimer) AddTransferredBytes(n int) {
	if n <= 0 {
		return
	}
	t.transferredBytes.Add(int64(n))
}

func (t *IdleTimer) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer t.ticker.Stop()
	t.publishLastRequestTimestamp()

	for {
		select {
		case <-t.ticker.C:
			transferred := t.transferredBytes.Load()

			// transfer(s) in progress
			if transferred > t.previousTickTransferredBytes {
				t.lastRequestTimestamp = time.Now()
				t.logger.Info("TTL control: connection(s) in progress", "bytes_transferred", transferred-t.previousTickTransferredBytes)
				t.previousTickTransferredBytes = transferred
				t.publishLastRequestTimestamp()
			} else {
				t.logger.Info("TTL control: connection(s) freezed")
				if (time.Since(t.lastRequestTimestamp) > t.timeToLive) && !t.expired {
					t.logger.Info("TTL control: DataManager time-to-live expired")
					// This send cannot block: ExpiredChan is buffered by one and the expired flag below
					// makes the send happen at most once, so the buffer always has room for it.
					t.ExpiredChan <- time.Now()
					t.expired = true
				}
			}

		case <-ctx.Done():
			return
		}
	}
}

// publishLastRequestTimestamp hands the last-activity timestamp to the accessTimestamp updater. The send is
// non-blocking on purpose: TickerChan is buffered by one, and a consumer still busy with a slow or hanging
// status update must not be able to stop this loop — a send that waits here freezes both the accessTimestamp
// refresh and the expiry itself, for good. Dropping a tick costs nothing, because the next tick of an
// ongoing transfer carries a newer timestamp.
func (t *IdleTimer) publishLastRequestTimestamp() {
	select {
	case t.TickerChan <- t.lastRequestTimestamp:
	default:
		t.logger.Info("TTL control: previous access timestamp update is still in flight, tick skipped")
	}
}
