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
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/deckhouse/storage-foundation/images/controller/pkg/config"
)

// AddVolumeCaptureRequestControllerToManager adds VolumeCaptureRequestController and its garbage-collection
// controller to the manager. The GC controller (cron-triggered, leader-only via the manager) replaces the
// former per-controller TTL scanner: it deletes terminal VolumeCaptureRequests after their TTL.
func AddVolumeCaptureRequestControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	apiReader := mgr.GetAPIReader()
	if apiReader == nil {
		return fmt.Errorf("APIReader must not be nil: controllers require APIReader to read StorageClass")
	}

	reconciler := &VolumeCaptureRequestController{
		Client:    mgr.GetClient(),
		APIReader: apiReader,
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return err
	}

	return SetupVCRGC(mgr)
}

// AddVolumeRestoreRequestControllerToManager adds VolumeRestoreRequestController and its garbage-collection
// controller to the manager. The GC controller replaces the former per-controller TTL scanner.
func AddVolumeRestoreRequestControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	apiReader := mgr.GetAPIReader()
	if apiReader == nil {
		return fmt.Errorf("APIReader must not be nil: controllers require APIReader to read StorageClass")
	}

	reconciler := &VolumeRestoreRequestController{
		Client:    mgr.GetClient(),
		APIReader: apiReader,
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return err
	}

	return SetupVRRGC(mgr)
}
