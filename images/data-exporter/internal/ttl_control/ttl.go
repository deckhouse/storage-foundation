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

	"github.com/deckhouse/storage-foundation/common"
)

type TTLControl struct {
	tsUpdater     DataManagerAccessTimestampUpdater
	expiredSetter DataManagerStatusExpiredSetter
	operation     common.Operation
	namespace     string
	name          string
	ttl           time.Duration
	logger        *slog.Logger
	idleTimer     *IdleTimer
}

func NewTTLControl(
	operation common.Operation,
	ttlStr string,
	namespace string,
	name string,
	tsUpdater DataManagerAccessTimestampUpdater,
	expiredSetter DataManagerStatusExpiredSetter,
	logger *slog.Logger,
) (*TTLControl, error) {
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		logger.Error("invalid program argument ", "ttl", ttlStr)
		return nil, err
	}

	idleTimer := NewIdleTimer(ttl, logger)

	return &TTLControl{
		tsUpdater:     tsUpdater,
		expiredSetter: expiredSetter,
		operation:     operation,
		namespace:     namespace,
		name:          name,
		ttl:           ttl,
		logger:        logger,
		idleTimer:     &idleTimer,
	}, nil
}

func (t *TTLControl) Start(ctx context.Context, handler http.Handler, wg *sync.WaitGroup) http.Handler {
	wg.Add(3)
	go t.idleTimer.Run(ctx, wg)
	go UpdateDataManagerAccessTimestamp(ctx, t.operation, t.tsUpdater, t.namespace, t.name, t.idleTimer.TickerChan, t.logger, wg)
	go SetDataManagerStatusExpired(ctx, t.operation, t.expiredSetter, t.namespace, t.name, t.idleTimer.ExpiredChan, t.logger, wg)

	return RequestNotifier(handler, t.idleTimer.RequestNotifierChan)
}
