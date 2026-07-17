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
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// ReconcileGCManager is the per-resource contract a caller implements to garbage-collect one kind:
//   - New returns a fresh empty object of the managed kind (used for Get and the controller's For()).
//   - ShouldBeDeleted decides whether a single object must be deleted now (typically: terminal phase and
//     age beyond TTL).
//   - ListForDelete returns, on each cron tick, all objects that should be deleted (typically DefaultFilter
//     applied to a List of the kind).
type ReconcileGCManager interface {
	New() client.Object
	ShouldBeDeleted(obj client.Object) bool
	ListForDelete(ctx context.Context, now time.Time) ([]client.Object, error)
}

// SetupGcController wires a cron-triggered garbage-collection controller for a single kind onto mgr.
func SetupGcController(
	controllerName string,
	mgr manager.Manager,
	log logr.Logger,
	schedule string,
	gcMgr ReconcileGCManager,
) error {
	log = log.WithValues("controller", controllerName)

	cronSource, err := NewCronSource(schedule, NewObjectLister(gcMgr.ListForDelete), log)
	if err != nil {
		return err
	}

	reconciler := NewReconciler(mgr.GetClient(), cronSource, gcMgr)

	if err := reconciler.SetupWithManager(controllerName, mgr); err != nil {
		return err
	}

	log.Info("Initialized garbage collector controller")

	return nil
}
