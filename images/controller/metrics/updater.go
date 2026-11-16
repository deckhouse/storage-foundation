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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	// updatePeriod bounds how old a scraped value is.
	updatePeriod = 15 * time.Second
	// updateTimeout stops a stuck read of one update function well before the next update is due.
	updateTimeout = 5 * time.Second
)

var _ manager.LeaderElectionRunnable = (*updater)(nil)

// updateFunc replaces the series of a set of metrics with the current values.
type updateFunc func(ctx context.Context)

// updater recounts the metrics when it starts and then every updatePeriod.
type updater struct {
	updateFuncs []updateFunc
}

func newUpdater(updateFuncs []updateFunc) *updater {
	return &updater{updateFuncs: updateFuncs}
}

// Start updates the metrics until ctx is done.
func (u *updater) Start(ctx context.Context) error {
	ticker := time.NewTicker(updatePeriod)
	defer ticker.Stop()

	for {
		for _, update := range u.updateFuncs {
			runUpdate(ctx, update)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// NeedLeaderElection reports whether u runs only on the elected replica; it does, so a standby publishes no duplicates.
func (u *updater) NeedLeaderElection() bool {
	return true
}

// runUpdate calls update with a timeout of its own, so that a stuck update does not delay the
// others.
func runUpdate(ctx context.Context, update updateFunc) {
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	update(ctx)
}
