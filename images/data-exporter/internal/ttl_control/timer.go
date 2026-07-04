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
	"net/http"
	"sync"
	"time"
)

// Time period for wake up time-to-live timer and update DataExport/DataImport.Status.AccessTimestamp
const tickerInterval = 30 * time.Second

// Middleware used for time-to-live control:
// Generates messages to time-to_live timer with bytes amount, written to http.ResponseWriter
// If all connections finished or freezed (zero bytes was written in tickerInterval), the time-to-live countdown begins
// Each tickerInterval DataExport/DataImport.Status.AccessTimestamp fileld will be updated
// After time-to-live period will expired, Condition "Expired" will be updated to true
func RequestNotifier(next http.Handler, notifyChan chan int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// prohibit page cashing for update idle timer each user click
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		watcher := newResponseWatcher(w, notifyChan)
		next.ServeHTTP(&watcher, r)
	})
}

// Override method "write" in http.ResponseWriter for send written bytes number to time-to-live timer
type responseWatcher struct {
	http.ResponseWriter
	requestNotifierChan chan<- int
}

func newResponseWatcher(w http.ResponseWriter, requestNotifierChan chan<- int) responseWatcher {
	return responseWatcher{
		w,
		requestNotifierChan,
	}
}

func (rw *responseWatcher) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	if err == nil {
		rw.requestNotifierChan <- n
	}
	return n, err
}

type IdleTimer struct {
	RequestNotifierChan      chan int
	TickerChan               chan time.Time
	ExpiredChan              chan time.Time
	timeToLive               time.Duration
	previousTickWrittenBytes int
	currentTickWrittenBytes  int
	lastRequestTimestamp     time.Time
	ticker                   *time.Ticker
	expired                  bool
	logger                   *slog.Logger
}

func NewIdleTimer(timeToLive time.Duration, logger *slog.Logger) IdleTimer {
	return IdleTimer{
		RequestNotifierChan:      make(chan int, 32),
		TickerChan:               make(chan time.Time, 1),
		ExpiredChan:              make(chan time.Time, 1),
		timeToLive:               timeToLive,
		previousTickWrittenBytes: 0,
		currentTickWrittenBytes:  0,
		lastRequestTimestamp:     time.Now(),
		ticker:                   time.NewTicker(tickerInterval),
		expired:                  false,
		logger:                   logger,
	}
}

func (t *IdleTimer) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer t.ticker.Stop()
	t.TickerChan <- t.lastRequestTimestamp

	for {
		select {
		case newWrittenBytes, ok := <-t.RequestNotifierChan:
			if ok {
				t.currentTickWrittenBytes += newWrittenBytes
			} else {
				return
			}

		case <-t.ticker.C:

			// responseWriter in progress
			if t.currentTickWrittenBytes > t.previousTickWrittenBytes {
				t.lastRequestTimestamp = time.Now()
				t.logger.Info("TTL control: connection(s) in progress", "bytes_written", t.currentTickWrittenBytes-t.previousTickWrittenBytes)
				t.previousTickWrittenBytes = t.currentTickWrittenBytes
				t.TickerChan <- t.lastRequestTimestamp
			} else {
				t.logger.Info("TTL control: connection(s) freezed")
				if (time.Since(t.lastRequestTimestamp) > t.timeToLive) && !t.expired {
					t.logger.Info("TTL control: DataManager time-to-live expired")
					t.ExpiredChan <- time.Now()
					t.expired = true
				}
			}

		case <-ctx.Done():
			return
		}
	}
}
