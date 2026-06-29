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

const setStatusExiredRetryPeriod = 60 * time.Second

func SetDataManagerStatusExpired(
	ctx context.Context,
	operation common.Operation,
	client DataManagerStatusExpiredSetter,
	dataManagerNamespace, dataManagerName string,
	expiredChan <-chan time.Time,
	logger *slog.Logger,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	select {
	case <-expiredChan:
		for {
			err := client.SetStatusExpired(ctx, operation, dataManagerNamespace, dataManagerName)
			if err != nil {
				logger.Info("Setting Condition Expired failed", "error", err.Error())
			} else {
				return
			}

			select {
			case <-time.After(setStatusExiredRetryPeriod):
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
