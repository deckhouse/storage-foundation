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

// Package gc wires the generic common/gc collector to the execution objects DataImport and DataExport.
// Each object is deleted once it has been in a terminal phase (DataImport: Completed | Expired | Failed;
// DataExport: Expired | Failed) for longer than the configured TTL, measured from status.completionTimestamp
// (NOT creationTimestamp — a transfer may run for hours before completing). Deletion is a plain Delete: the
// object's finalizer then drives the controller's cleanup (which tears down infra and lets the ObjectKeeper
// bridge fall away), so an unadopted artifact is reclaimed by Kubernetes GC. Configuration is env-only.
package gc

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	commongc "github.com/deckhouse/storage-foundation/common/gc"
)

const (
	dataImportGCControllerName = "data-import-gc-controller"
	dataExportGCControllerName = "data-export-gc-controller"

	gcDataImportTTLEnv      = "GC_DATA_IMPORT_TTL"
	gcDataImportScheduleEnv = "GC_DATA_IMPORT_SCHEDULE"
	gcDataExportTTLEnv      = "GC_DATA_EXPORT_TTL"
	gcDataExportScheduleEnv = "GC_DATA_EXPORT_SCHEDULE"

	// defaultGCTTL keeps a terminal object for a day — for a Completed DataImport this is the explicit
	// artifact-adoption window; for Expired/Failed it is simply cleanup lag.
	defaultGCTTL = 24 * time.Hour
	// defaultGCSchedule sweeps hourly, so worst-case retention is TTL + one sweep (~25h).
	defaultGCSchedule = "0 * * * *"
)

func defaultGcSettings() commongc.BaseGcSettings {
	return commongc.BaseGcSettings{
		TTL:      metav1.Duration{Duration: defaultGCTTL},
		Schedule: defaultGCSchedule,
	}
}

// SetupDataImportGC registers the DataImport garbage-collection controller.
func SetupDataImportGC(mgr manager.Manager) error {
	settings, err := commongc.GetBaseGcSettingsFromEnv(gcDataImportScheduleEnv, gcDataImportTTLEnv, defaultGcSettings())
	if err != nil {
		return err
	}
	m := &dataImportGCManager{client: mgr.GetClient(), ttl: settings.TTL.Duration, now: time.Now}
	return commongc.SetupGcController(dataImportGCControllerName, mgr, mgr.GetLogger().WithValues("resource", "dataimport"), settings.Schedule, m)
}

// SetupDataExportGC registers the DataExport garbage-collection controller.
func SetupDataExportGC(mgr manager.Manager) error {
	settings, err := commongc.GetBaseGcSettingsFromEnv(gcDataExportScheduleEnv, gcDataExportTTLEnv, defaultGcSettings())
	if err != nil {
		return err
	}
	m := &dataExportGCManager{client: mgr.GetClient(), ttl: settings.TTL.Duration, now: time.Now}
	return commongc.SetupGcController(dataExportGCControllerName, mgr, mgr.GetLogger().WithValues("resource", "dataexport"), settings.Schedule, m)
}

// isReapable reports whether a terminal, non-terminating object whose completionTimestamp is older than
// ttl should be deleted. It is shared by DataImport and DataExport (both embed DataExportImportStatus and
// use the common.Phase catalog); a DataExport simply never reaches the Completed phase.
func isReapable(obj client.Object, status *dev1alpha1.DataExportImportStatus, ttl time.Duration, now time.Time) bool {
	if !obj.GetDeletionTimestamp().IsZero() {
		return false
	}
	if !common.Phase(status.Phase).IsTerminal() {
		return false
	}
	ct := status.CompletionTimestamp
	if ct == nil || ct.IsZero() {
		// Terminal but no completion time recorded — never reap (fail-safe; the GC clock is undefined).
		return false
	}
	return now.Sub(ct.Time) > ttl
}

// completionTime is the common/gc AgeTimeFunc: the object's terminal time, or zero when not yet terminal.
func completionTime(status *dev1alpha1.DataExportImportStatus) time.Time {
	if status.CompletionTimestamp == nil {
		return time.Time{}
	}
	return status.CompletionTimestamp.Time
}

// ---- DataImport ----

var _ commongc.ReconcileGCManager = &dataImportGCManager{}

type dataImportGCManager struct {
	client client.Client
	ttl    time.Duration
	now    func() time.Time
}

func (m *dataImportGCManager) New() client.Object { return &dev1alpha1.DataImport{} }

func (m *dataImportGCManager) ShouldBeDeleted(obj client.Object) bool {
	di, ok := obj.(*dev1alpha1.DataImport)
	if !ok {
		return false
	}
	return isReapable(di, &di.Status, m.ttl, m.now())
}

func (m *dataImportGCManager) ListForDelete(ctx context.Context, now time.Time) ([]client.Object, error) {
	list := &dev1alpha1.DataImportList{}
	if err := m.client.List(ctx, list); err != nil {
		return nil, err
	}
	objs := make([]client.Object, 0, len(list.Items))
	for i := range list.Items {
		objs = append(objs, &list.Items[i])
	}
	isTerminal := func(obj client.Object) bool {
		di, ok := obj.(*dev1alpha1.DataImport)
		return ok && common.Phase(di.Status.Phase).IsTerminal()
	}
	ageTime := func(obj client.Object) time.Time {
		di, ok := obj.(*dev1alpha1.DataImport)
		if !ok {
			return time.Time{}
		}
		return completionTime(&di.Status)
	}
	return commongc.DefaultFilter(objs, isTerminal, m.ttl, ageTime, nil, 0, now), nil
}

// ---- DataExport ----

var _ commongc.ReconcileGCManager = &dataExportGCManager{}

type dataExportGCManager struct {
	client client.Client
	ttl    time.Duration
	now    func() time.Time
}

func (m *dataExportGCManager) New() client.Object { return &dev1alpha1.DataExport{} }

func (m *dataExportGCManager) ShouldBeDeleted(obj client.Object) bool {
	de, ok := obj.(*dev1alpha1.DataExport)
	if !ok {
		return false
	}
	return isReapable(de, &de.Status, m.ttl, m.now())
}

func (m *dataExportGCManager) ListForDelete(ctx context.Context, now time.Time) ([]client.Object, error) {
	list := &dev1alpha1.DataExportList{}
	if err := m.client.List(ctx, list); err != nil {
		return nil, err
	}
	objs := make([]client.Object, 0, len(list.Items))
	for i := range list.Items {
		objs = append(objs, &list.Items[i])
	}
	isTerminal := func(obj client.Object) bool {
		de, ok := obj.(*dev1alpha1.DataExport)
		return ok && common.Phase(de.Status.Phase).IsTerminal()
	}
	ageTime := func(obj client.Object) time.Time {
		de, ok := obj.(*dev1alpha1.DataExport)
		if !ok {
			return time.Time{}
		}
		return completionTime(&de.Status)
	}
	return commongc.DefaultFilter(objs, isTerminal, m.ttl, ageTime, nil, 0, now), nil
}
