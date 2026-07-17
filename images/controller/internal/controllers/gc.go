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

package controllers

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	commongc "github.com/deckhouse/storage-foundation/common/gc"
)

// Garbage collection for VolumeCaptureRequest and VolumeRestoreRequest. This replaces the previous
// per-controller TTL scanner + informational TTL annotation: both request kinds are deleted once they have
// been terminal (Ready=True or Ready=False) for longer than the configured TTL, measured from
// status.completionTimestamp. Deletion is a plain Delete — a VCR's ObjectKeeper (FollowObject) then reaps
// the owned artifacts via ownerRef, and the VCR GC additionally cleans up any artifact left without an
// ownerRef (PreDelete). Configuration is env-only.
const (
	vcrGCControllerName = "volumecapturerequest-gc-controller"
	vrrGCControllerName = "volumerestorerequest-gc-controller"

	gcVCRTTLEnv      = "GC_VCR_TTL"
	gcVCRScheduleEnv = "GC_VCR_SCHEDULE"
	gcVRRTTLEnv      = "GC_VRR_TTL"
	gcVRRScheduleEnv = "GC_VRR_SCHEDULE"

	defaultGCTTL      = 24 * time.Hour
	defaultGCSchedule = "0 * * * *"
)

func defaultGcSettings() commongc.BaseGcSettings {
	return commongc.BaseGcSettings{
		TTL:      metav1.Duration{Duration: defaultGCTTL},
		Schedule: defaultGCSchedule,
	}
}

// SetupVCRGC registers the VolumeCaptureRequest garbage-collection controller.
func SetupVCRGC(mgr manager.Manager) error {
	settings, err := commongc.GetBaseGcSettingsFromEnv(gcVCRScheduleEnv, gcVCRTTLEnv, defaultGcSettings())
	if err != nil {
		return err
	}
	m := &vcrGCManager{client: mgr.GetClient(), ttl: settings.TTL.Duration, now: time.Now}
	return commongc.SetupGcController(vcrGCControllerName, mgr, mgr.GetLogger().WithValues("resource", "volumecapturerequest"), settings.Schedule, m)
}

// SetupVRRGC registers the VolumeRestoreRequest garbage-collection controller.
func SetupVRRGC(mgr manager.Manager) error {
	settings, err := commongc.GetBaseGcSettingsFromEnv(gcVRRScheduleEnv, gcVRRTTLEnv, defaultGcSettings())
	if err != nil {
		return err
	}
	m := &vrrGCManager{client: mgr.GetClient(), ttl: settings.TTL.Duration, now: time.Now}
	return commongc.SetupGcController(vrrGCControllerName, mgr, mgr.GetLogger().WithValues("resource", "volumerestorerequest"), settings.Schedule, m)
}

func agedOut(completion *metav1.Time, ttl time.Duration, now time.Time) bool {
	if completion == nil || completion.IsZero() {
		// Terminal objects always carry a completionTimestamp; a missing one is a fail-safe no-op.
		return false
	}
	return now.Sub(completion.Time) > ttl
}

// ---- VolumeCaptureRequest ----

var (
	_ commongc.ReconcileGCManager = &vcrGCManager{}
	_ commongc.PreDeleter         = &vcrGCManager{}
)

type vcrGCManager struct {
	client client.Client
	ttl    time.Duration
	now    func() time.Time
}

func (m *vcrGCManager) New() client.Object { return &storagev1alpha1.VolumeCaptureRequest{} }

func (m *vcrGCManager) ShouldBeDeleted(obj client.Object) bool {
	vcr, ok := obj.(*storagev1alpha1.VolumeCaptureRequest)
	if !ok {
		return false
	}
	return vcr.DeletionTimestamp.IsZero() &&
		isVolumeCaptureTerminal(vcr.Status.Conditions) &&
		agedOut(vcr.Status.CompletionTimestamp, m.ttl, m.now())
}

func (m *vcrGCManager) ListForDelete(ctx context.Context, now time.Time) ([]client.Object, error) {
	list := &storagev1alpha1.VolumeCaptureRequestList{}
	if err := m.client.List(ctx, list); err != nil {
		return nil, err
	}
	objs := make([]client.Object, 0, len(list.Items))
	for i := range list.Items {
		objs = append(objs, &list.Items[i])
	}
	isTerminalCandidate := func(obj client.Object) bool {
		vcr, ok := obj.(*storagev1alpha1.VolumeCaptureRequest)
		return ok && isVolumeCaptureTerminal(vcr.Status.Conditions)
	}
	ageTime := func(obj client.Object) time.Time {
		vcr, ok := obj.(*storagev1alpha1.VolumeCaptureRequest)
		if !ok || vcr.Status.CompletionTimestamp == nil {
			return time.Time{}
		}
		return vcr.Status.CompletionTimestamp.Time
	}
	return commongc.DefaultFilter(objs, isTerminalCandidate, m.ttl, ageTime, nil, 0, now), nil
}

// PreDelete reaps a VCR artifact that has no ownerReferences at all (a true orphan the ownerRef cascade
// cannot reach) before the VCR is deleted, so it is not leaked. It is BEST-EFFORT: a failure (e.g. missing
// RBAC to delete a PersistentVolume artifact) is logged but not returned, so the VCR is still collected
// instead of wedging in an error-requeue loop — matching the previous scanner's best-effort cleanup.
func (m *vcrGCManager) PreDelete(ctx context.Context, obj client.Object) error {
	vcr, ok := obj.(*storagev1alpha1.VolumeCaptureRequest)
	if !ok {
		return nil
	}
	if err := cleanupVCRArtifacts(ctx, m.client, vcr); err != nil {
		log.FromContext(ctx).Error(err, "best-effort orphan-artifact cleanup failed; garbage-collecting VCR anyway",
			"namespace", vcr.Namespace, "name", vcr.Name)
	}
	return nil
}

// ---- VolumeRestoreRequest ----

var _ commongc.ReconcileGCManager = &vrrGCManager{}

type vrrGCManager struct {
	client client.Client
	ttl    time.Duration
	now    func() time.Time
}

func (m *vrrGCManager) New() client.Object { return &storagev1alpha1.VolumeRestoreRequest{} }

func (m *vrrGCManager) ShouldBeDeleted(obj client.Object) bool {
	vrr, ok := obj.(*storagev1alpha1.VolumeRestoreRequest)
	if !ok {
		return false
	}
	return vrr.DeletionTimestamp.IsZero() &&
		isTerminal(vrr.Status.Conditions, storagev1alpha1.ConditionTypeReady) &&
		agedOut(vrr.Status.CompletionTimestamp, m.ttl, m.now())
}

func (m *vrrGCManager) ListForDelete(ctx context.Context, now time.Time) ([]client.Object, error) {
	list := &storagev1alpha1.VolumeRestoreRequestList{}
	if err := m.client.List(ctx, list); err != nil {
		return nil, err
	}
	objs := make([]client.Object, 0, len(list.Items))
	for i := range list.Items {
		objs = append(objs, &list.Items[i])
	}
	isTerminalCandidate := func(obj client.Object) bool {
		vrr, ok := obj.(*storagev1alpha1.VolumeRestoreRequest)
		return ok && isTerminal(vrr.Status.Conditions, storagev1alpha1.ConditionTypeReady)
	}
	ageTime := func(obj client.Object) time.Time {
		vrr, ok := obj.(*storagev1alpha1.VolumeRestoreRequest)
		if !ok || vrr.Status.CompletionTimestamp == nil {
			return time.Time{}
		}
		return vrr.Status.CompletionTimestamp.Time
	}
	return commongc.DefaultFilter(objs, isTerminalCandidate, m.ttl, ageTime, nil, 0, now), nil
}
