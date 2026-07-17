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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

/*
CronSource is an implementation of the controller-runtime Source interface. It periodically triggers and
emits reconcile events for a caller-provided list of objects.

The component is independent of the Kubernetes client: the caller implements the ObjectLister interface,
which CronSource invokes on each tick to determine what to enqueue.
*/

var _ source.Source = &CronSource{}

const sourceName = "CronSource"

func NewCronSource(scheduleSpec string, objLister ObjectLister, log logr.Logger) (*CronSource, error) {
	schedule, err := cron.ParseStandard(scheduleSpec)
	if err != nil {
		return nil, fmt.Errorf("parsing standard spec %q: %w", scheduleSpec, err)
	}

	return &CronSource{
		schedule:  schedule,
		objLister: objLister,
		log:       log.WithValues("watchSource", sourceName),
		clock:     &clock.RealClock{},
	}, nil
}

type CronSource struct {
	schedule  cron.Schedule
	objLister ObjectLister
	log       logr.Logger
	clock     clock.Clock
}

func (c *CronSource) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	nextTime := nextScheduleTimeDuration(c.schedule, c.clock.Now())
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.clock.After(nextTime):
				c.enqueueObjects(ctx, queue.Add)
				nextTime = nextScheduleTimeDuration(c.schedule, c.clock.Now())
			}
		}
	}()
	return nil
}

func (c *CronSource) enqueueObjects(ctx context.Context, queueAddFunc func(reconcile.Request)) {
	now := c.clock.Now()
	objs, err := c.objLister.List(ctx, now)
	if err != nil {
		c.log.Error(err, "Failed to get ObjectList for delete")
		return
	}

	if len(objs) == 0 {
		c.log.V(1).Info("No resources to garbage-collect, skip queueing", "now", now)
		return
	}

	for _, obj := range objs {
		queueAddFunc(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: obj.GetNamespace(),
				Name:      obj.GetName(),
			},
		})
		c.log.V(1).Info("Resource enqueued for garbage collection", "namespace", obj.GetNamespace(), "name", obj.GetName())
	}
}

func nextScheduleTimeDuration(schedule cron.Schedule, now time.Time) time.Duration {
	return schedule.Next(now).Sub(now)
}

type ObjectLister interface {
	List(ctx context.Context, now time.Time) ([]client.Object, error)
}

type ObjectListerImpl struct {
	ListFunc func(ctx context.Context, now time.Time) ([]client.Object, error)
}

func (o *ObjectListerImpl) List(ctx context.Context, now time.Time) ([]client.Object, error) {
	if o.ListFunc == nil {
		return nil, nil
	}
	return o.ListFunc(ctx, now)
}

func NewObjectLister(listFunc func(ctx context.Context, now time.Time) ([]client.Object, error)) *ObjectListerImpl {
	return &ObjectListerImpl{listFunc}
}

func NewSingleObjectLister(namespace, name string) *ObjectListerImpl {
	return &ObjectListerImpl{ListFunc: func(_ context.Context, _ time.Time) ([]client.Object, error) {
		return []client.Object{&metav1.PartialObjectMetadata{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      name,
			},
		}}, nil
	}}
}
