/*
Copyright 2024 Flant JSC

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

package main

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"

	v1 "k8s.io/api/core/v1"
	sv1 "k8s.io/api/storage/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "fox.flant.com/deckhouse/storage/storage-foundation/api/v1alpha1"
	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/internal/controllers"
	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/config"
	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/kubutils"
	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/logger"
	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

var (
	resourcesSchemeFuncs = []func(*apiruntime.Scheme) error{
		v1alpha1.AddToScheme,
		deckhousev1alpha1.AddToScheme, // Add ObjectKeeper
		clientgoscheme.AddToScheme,
		extv1.AddToScheme,
		v1.AddToScheme,
		sv1.AddToScheme,
		snapshotv1.AddToScheme, // Add CSI VolumeSnapshot types
	}
)

func main() {
	ctx := context.Background()
	cfgParams := config.NewConfig()

	log, err := logger.NewLogger(cfgParams.Loglevel)
	if err != nil {
		fmt.Printf("unable to create NewLogger, err: %v\n", err)
		os.Exit(1)
	}

	log.Info(fmt.Sprintf("[main] Go Version:%s ", goruntime.Version()))
	log.Info(fmt.Sprintf("[main] OS/Arch:Go OS/Arch:%s/%s ", goruntime.GOOS, goruntime.GOARCH))

	log.Info("[main] CfgParams has been successfully created")
	log.Info(fmt.Sprintf("[main] %s = %s", config.LogLevelEnvName, cfgParams.Loglevel))

	kConfig, err := kubutils.KubernetesDefaultConfigCreate()
	if err != nil {
		log.Error(err, "[main] unable to KubernetesDefaultConfigCreate")
	}
	log.Info("[main] kubernetes config has been successfully created.")

	scheme := apiruntime.NewScheme()
	for _, f := range resourcesSchemeFuncs {
		err := f(scheme)
		if err != nil {
			log.Error(err, "[main] unable to add scheme to func")
			os.Exit(1)
		}
	}
	log.Info("[main] successfully read scheme CR")

	// Set logger for controller-runtime BEFORE creating manager
	// This prevents the warning: "[controller-runtime] log.SetLogger(...) was never called; logs will not be displayed"
	ctrl.SetLogger(log.GetLogger())

	// NOTE: We don't restrict cache to specific namespaces because VolumeCaptureRequest
	// and VolumeRestoreRequest can be created in any namespace by users.
	// The controller needs to watch all namespaces to process these resources.
	managerOpts := manager.Options{
		Scheme: scheme,
		//MetricsBindAddress: cfgParams.MetricsPort,
		HealthProbeBindAddress:  cfgParams.HealthProbeBindAddress,
		LeaderElection:          true,
		LeaderElectionNamespace: cfgParams.ControllerNamespace,
		LeaderElectionID:        config.ControllerName,
		Logger:                  log.GetLogger(),
	}

	mgr, err := manager.New(kConfig, managerOpts)
	if err != nil {
		log.Error(err, "[main] unable to manager.New")
		os.Exit(1)
	}
	log.Info("[main] successfully created kubernetes manager")

	// Add VolumeCaptureRequest controller
	if err = controllers.AddVolumeCaptureRequestControllerToManager(mgr, cfgParams); err != nil {
		log.Error(err, "unable to create controller", "controller", "VolumeCaptureRequest")
		os.Exit(1)
	}
	log.Info("VolumeCaptureRequestController added to manager")

	// Add VolumeRestoreRequest controller
	if err = controllers.AddVolumeRestoreRequestControllerToManager(mgr, cfgParams); err != nil {
		log.Error(err, "unable to create controller", "controller", "VolumeRestoreRequest")
		os.Exit(1)
	}
	log.Info("VolumeRestoreRequestController added to manager")

	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "[main] unable to mgr.AddHealthzCheck")
		os.Exit(1)
	}
	log.Info("[main] successfully AddHealthzCheck")

	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "[main] unable to mgr.AddReadyzCheck")
		os.Exit(1)
	}
	log.Info("[main] successfully AddReadyzCheck")

	err = mgr.Start(ctx)
	if err != nil {
		log.Error(err, "[main] unable to mgr.Start")
		os.Exit(1)
	}
}
