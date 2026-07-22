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

package gc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestNextScheduleTimeDuration(t *testing.T) {
	schedule, err := cron.ParseStandard("0 * * * *") // top of every hour
	require.NoError(t, err)
	now := time.Date(2026, 7, 17, 12, 30, 0, 0, time.UTC)
	// Next tick after 12:30 is 13:00 → 30 minutes away.
	assert.Equal(t, 30*time.Minute, nextScheduleTimeDuration(schedule, now))
}

func TestCronSource_enqueueObjects(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	newCS := func(lister ObjectLister) *CronSource {
		return &CronSource{objLister: lister, log: logr.Discard(), clock: clocktesting.NewFakeClock(now)}
	}
	partial := func(ns, name string) client.Object {
		return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}

	t.Run("N objects → N requests with correct ns/name", func(t *testing.T) {
		lister := NewObjectLister(func(context.Context, time.Time) ([]client.Object, error) {
			return []client.Object{partial("ns", "a"), partial("ns", "b")}, nil
		})
		var got []reconcile.Request
		newCS(lister).enqueueObjects(context.Background(), func(r reconcile.Request) { got = append(got, r) })
		assert.ElementsMatch(t, []reconcile.Request{
			{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "a"}},
			{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "b"}},
		}, got)
	})

	t.Run("empty list → nothing enqueued", func(t *testing.T) {
		lister := NewObjectLister(func(context.Context, time.Time) ([]client.Object, error) { return nil, nil })
		count := 0
		newCS(lister).enqueueObjects(context.Background(), func(reconcile.Request) { count++ })
		assert.Zero(t, count)
	})

	t.Run("list error → nothing enqueued, no panic", func(t *testing.T) {
		lister := NewObjectLister(func(context.Context, time.Time) ([]client.Object, error) {
			return nil, errors.New("list boom")
		})
		count := 0
		assert.NotPanics(t, func() {
			newCS(lister).enqueueObjects(context.Background(), func(reconcile.Request) { count++ })
		})
		assert.Zero(t, count)
	})
}

// TestCronSource_Start_dispatchesOnTick drives the cron loop deterministically with a FakeClock: after the
// scheduled interval elapses, the listed object is enqueued into the real workqueue. Cancelling the context
// terminates the goroutine.
func TestCronSource_Start_dispatchesOnTick(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 30, 0, 0, time.UTC)
	fc := clocktesting.NewFakeClock(now)
	schedule, err := cron.ParseStandard("0 * * * *")
	require.NoError(t, err)

	cs := &CronSource{
		schedule:  schedule,
		objLister: NewSingleObjectLister("ns", "obj"),
		log:       logr.Discard(),
		clock:     fc,
	}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, cs.Start(ctx, queue))

	// The Start goroutine registers a timer waiter via clock.After before the first tick fires.
	require.Eventually(t, fc.HasWaiters, time.Second, time.Millisecond, "Start must register a timer waiter")

	// Advance to the next scheduled tick (12:30 → 13:00); the tick lists and enqueues the object.
	fc.Step(nextScheduleTimeDuration(schedule, now))
	require.Eventually(t, func() bool { return queue.Len() == 1 }, time.Second, time.Millisecond,
		"a cron tick must enqueue the listed object exactly once")

	item, _ := queue.Get()
	assert.Equal(t, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "obj"}}, item)

	// Cancelling the context terminates the goroutine (best-effort cleanup; no further ticks are asserted).
	cancel()
}
