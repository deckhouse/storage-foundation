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
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/config"
)

// AddVolumeCaptureRequestControllerToManager adds VolumeCaptureRequestController to the manager
// and starts TTL scanner as a leader-only runnable.
//
// TTL scanner runs only on the leader replica to prevent duplicate deletion attempts.
// When leadership changes, the scanner context is cancelled and scanner stops gracefully.
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

	// Start TTL scanner as leader-only runnable
	// RunnableFunc is called only on the leader replica
	// When leadership changes, ctx.Done() triggers graceful shutdown
	// The scanner goroutine will exit when context is cancelled
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		reconciler.StartTTLScanner(ctx, mgr.GetClient())
		// StartTTLScanner starts scanner in goroutine and returns immediately
		// We need to wait for context cancellation to keep Runnable alive
		<-ctx.Done()
		return nil
	})); err != nil {
		return err
	}

	return nil
}

// AddVolumeRestoreRequestControllerToManager adds VolumeRestoreRequestController to the manager
func AddVolumeRestoreRequestControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	apiReader := mgr.GetAPIReader()
	if apiReader == nil {
		return fmt.Errorf("APIReader must not be nil: controllers require APIReader to read StorageClass")
	}

	return (&VolumeRestoreRequestController{
		Client:    mgr.GetClient(),
		APIReader: apiReader,
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
	}).SetupWithManager(mgr)
}
