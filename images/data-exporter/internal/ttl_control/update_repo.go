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
	"time"

	"github.com/deckhouse/storage-foundation/common"
)

const setServerStateIdleExpiredRetryPeriod = 60 * time.Second

// SetDataManagerServerStateIdleExpired waits for the idle timer to fire (idle >= ttl with no in-flight
// transfer) and then publishes status.serverState = IdleExpired. It retries on failure so the signal is
// durable; the controller turns IdleExpired into the terminal Expired phase. SetServerState itself
// guards against clobbering a Finished import, so a late idle-expiry after a completed upload is a no-op.
func SetDataManagerServerStateIdleExpired(
	ctx context.Context,
	operation common.Operation,
	client DataManagerServerStateSetter,
	dataManagerNamespace, dataManagerName string,
	expiredChan <-chan time.Time,
	logger *slog.Logger,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	select {
	case <-expiredChan:
		for {
			err := client.SetServerState(ctx, operation, dataManagerNamespace, dataManagerName, common.ServerStateIdleExpired)
			if err != nil {
				logger.Info("Setting serverState IdleExpired failed", "error", err.Error())
			} else {
				return
			}

			select {
			case <-time.After(setServerStateIdleExpiredRetryPeriod):
			case <-ctx.Done():
				return
			}
		}
	case <-ctx.Done():
		return
	}
}

func UpdateDataManagerAccessTimestamp(
	ctx context.Context,
	operation common.Operation,
	client DataManagerAccessTimestampUpdater,
	dataManagerNamespace, dataManagerName string,
	signalChan <-chan time.Time,
	logger *slog.Logger,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	for {
		select {
		case timestamp, ok := <-signalChan:
			if ok {
				err := client.UpdateAccessTimestamp(ctx, operation, dataManagerNamespace, dataManagerName, timestamp)
				if err != nil {
					logger.Info("Update Access Timestamp failed", "error", err.Error())
				}
			} else {
				return
			}

		case <-ctx.Done():
			return
		}
	}
}
