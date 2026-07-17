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
	"strings"
	"sync"
	"time"

	"github.com/deckhouse/storage-foundation/common"
)

// defaultServerTTL is the idle-TTL fallback used when the server is started without an explicit --ttl
// (empty spec.ttl). spec.ttl is required by the CRD, but its pattern also matches the empty string, so a
// producer that sets ttl:"" would otherwise crash the pod on ParseDuration(""). Falling back to the
// documented default (1m) keeps the server serving and simply enforces a conservative idle window instead
// of crash-looping.
const defaultServerTTL = time.Minute

type TTLControl struct {
	tsUpdater         DataManagerAccessTimestampUpdater
	serverStateSetter DataManagerServerStateSetter
	operation         common.Operation
	namespace         string
	name              string
	ttl               time.Duration
	logger            *slog.Logger
	idleTimer         *IdleTimer
}

func NewTTLControl(
	operation common.Operation,
	ttlStr string,
	namespace string,
	name string,
	tsUpdater DataManagerAccessTimestampUpdater,
	serverStateSetter DataManagerServerStateSetter,
	logger *slog.Logger,
) (*TTLControl, error) {
	// Fallback for an empty --ttl (empty spec.ttl): use the documented default instead of failing
	// ParseDuration and crash-looping the server pod.
	if strings.TrimSpace(ttlStr) == "" {
		logger.Warn("empty ttl argument; falling back to the default idle window", "default", defaultServerTTL.String())
		ttlStr = defaultServerTTL.String()
	}

	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		logger.Error("invalid program argument ", "ttl", ttlStr)
		return nil, err
	}

	idleTimer := NewIdleTimer(ttl, logger)

	return &TTLControl{
		tsUpdater:         tsUpdater,
		serverStateSetter: serverStateSetter,
		operation:         operation,
		namespace:         namespace,
		name:              name,
		ttl:               ttl,
		logger:            logger,
		idleTimer:         &idleTimer,
	}, nil
}

func (t *TTLControl) Start(ctx context.Context, handler http.Handler, wg *sync.WaitGroup) http.Handler {
	wg.Add(3)
	go t.idleTimer.Run(ctx, wg)
	go UpdateDataManagerAccessTimestamp(ctx, t.operation, t.tsUpdater, t.namespace, t.name, t.idleTimer.TickerChan, t.logger, wg)
	go SetDataManagerServerStateIdleExpired(ctx, t.operation, t.serverStateSetter, t.namespace, t.name, t.idleTimer.ExpiredChan, t.logger, wg)

	return RequestNotifier(handler, t.idleTimer.RequestNotifierChan)
}
