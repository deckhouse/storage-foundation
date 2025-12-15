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
	ctrl "sigs.k8s.io/controller-runtime"

	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/config"
)

// AddVolumeCaptureRequestControllerToManager adds VolumeCaptureRequestController to the manager
func AddVolumeCaptureRequestControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	return (&VolumeCaptureRequestController{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: cfg,
	}).SetupWithManager(mgr)
}

// AddVolumeRestoreRequestControllerToManager adds VolumeRestoreRequestController to the manager
func AddVolumeRestoreRequestControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	return (&VolumeRestoreRequestController{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: cfg,
	}).SetupWithManager(mgr)
}
