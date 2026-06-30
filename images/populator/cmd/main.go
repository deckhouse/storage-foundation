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

package main

import (
	"context"
	stdlog "log"
	"net/http"
	"os"

	"github.com/go-logr/stdr"
	populatorMachinery "github.com/kubernetes-csi/lib-volume-populator/v3/populator-machinery"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

const healthProbeAddr = ":8081"

var (
	cfgParams *config.Options
	cfg       *rest.Config
	cl        client.Client
)

func main() {
	stdr.SetVerbosity(5)
	log.SetLogger(stdr.New(stdlog.New(os.Stdout, "___", stdlog.LstdFlags)))

	log.Log.Info("Run provider-populator controller")

	cfgParams = config.NewConfig()

	pfcfg := &populatorMachinery.ProviderFunctionConfig{
		PopulateFn:         populateFn,
		PopulateCompleteFn: populateCompleteFn,
		PopulateCleanupFn:  populateCleanupFn,
	}

	groupKind := schema.GroupKind{
		Group: "storage-foundation.deckhouse.io", // TODO: move this and other values to constants/config/etc
		Kind:  "DataImport",
	}
	versionResource := schema.GroupVersionResource{
		Group:    groupKind.Group,
		Version:  "v1alpha1",
		Resource: "dataimports",
	}

	vpcfg := &populatorMachinery.VolumePopulatorConfig{
		MasterURL:  "",
		Kubeconfig: os.Getenv("KUBECONFIG"),
		// HttpEndpoint:           *httpEndpoint, // ???
		// MetricsPath:            *metricsPath,  // ???
		Namespace:              cfgParams.ControllerNamespace,
		Prefix:                 "storage-foundation.deckhouse.io",
		Gk:                     groupKind,
		Gvr:                    versionResource,
		ProviderFunctionConfig: pfcfg,
		CrossNamespace:         false,
	}

	// We have to read Kubeconfig  again, because config from populatorMachinery is unuaccessible
	var err error
	cfg, err = clientcmd.BuildConfigFromFlags(vpcfg.MasterURL, vpcfg.Kubeconfig)
	if err != nil {
		log.Log.Error(err, "Failed to create config")
		return
	}

	// Initialize scheme with our CRDs and core Kubernetes types used by the populator
	scheme := runtime.NewScheme()
	_ = dev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	cl, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Log.Error(err, "Failed to create client")
		return
	}

	// Start health probe server
	go startHealthProbeServer()

	populatorMachinery.RunControllerWithConfig(*vpcfg)
}

func startHealthProbeServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:    healthProbeAddr,
		Handler: mux,
	}

	log.Log.Info("Starting health probe server", "addr", healthProbeAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Log.Error(err, "Failed to start health probe server")
	}
}

func populateFn(ctx context.Context, params populatorMachinery.PopulatorParams) error {
	// Implement the provider-specific logic to initiate volume population.
	// This may involve calling cloud-native APIs or creating temporary Kubernetes resources
	// such as Pods or Jobs for data transfer.

	// Example steps:
	// 1. Retrieve data source details from the defined CRD.
	// 2. Initiate a data transfer job to params.PvcPrime using params.KubeClient.
	// 3. Report the volume population status to the original PVC's through params.Recorder.
	// 4. You should check if the transfer job already exists before creating it, otherwise the transfer job might
	// get created multiple times if everytime you use a unique name.
	logger := log.FromContext(ctx)
	logger.Info("called populateFn", "pvcNamespace", params.Pvc.Namespace, "pvcName", params.Pvc.Name)

	dataImport, names, err := getDataImportAndNames(ctx, params.Unstructured)
	if err != nil {
		logger.Error(err, "failed to get DataImport")
		return err
	}

	err = ensureDeployment(ctx, names, params, dataImport)
	if err != nil {
		logger.Error(err, "Failed to ensure Deployment")
		return err
	}

	return nil
}

func populateCompleteFn(ctx context.Context, params populatorMachinery.PopulatorParams) (bool, error) {
	// Implement the provider-specific logic to determine the status of volume population.
	// This may involve calling cloud-native APIs or checking the completion status of
	// temporary Kubernetes resources like Pods or Jobs.

	// Example steps:
	// 1. Fetch the transfer job using params.KubeClient.
	// 2. Verify if the job has finished successfully (returns true) or is still running (returns false).
	// 3. If the transfer job encountered an error, evaluate the need for cleanup.
	// 4. Report the volume population status to the original PVC through params.Recorder.
	logger := log.FromContext(ctx)
	logger.Info("called populateCompleteFn", "pvcNamespace", params.Pvc.Namespace, "pvcName", params.Pvc.Name)

	dataImport, names, err := getDataImportAndNames(ctx, params.Unstructured)
	if err != nil {
		logger.Error(err, "failed to get DataImport")
		return false, err
	}

	cond := common.GetCondition(dataImport.Status.Conditions, common.ConditionUploadFinished)
	if cond != nil && cond.Status == metav1.ConditionTrue {
		logger.Info("DataImport population is finished")
		return true, nil
	}

	condReady := common.GetCondition(dataImport.Status.Conditions, common.ConditionReady)
	if condReady != nil && (condReady.Reason == string(common.ReasonDeleted) || condReady.Reason == string(common.ReasonExpired)) {
		logger.Info("DataImport is deleted or expired, population is finished")
		return true, nil
	}

	// Ensure that deployment is still present
	err = ensureDeployment(ctx, names, params, dataImport)
	if err != nil {
		logger.Error(err, "Failed to ensure Deployment")
		return false, err
	}

	return false, nil
}

func populateCleanupFn(ctx context.Context, params populatorMachinery.PopulatorParams) error {
	// Implement the provider-specific logic to clean up any temporary resources
	// that were created during the volume population process. This step happens after PV rebind to the original PVC
	// and before the PVC' gets deleted.

	// Example steps:
	// 1. Fetch the transfer job using params.KubeClient.
	// 2. If the transfer job still exists delete the job.
	// 3. Report the volume population status to the original PVC through params.Recorder.
	logger := log.FromContext(ctx)
	logger.Info("called populateCleanupFn", "pvcNamespace", params.Pvc.Namespace, "pvcName", params.Pvc.Name)

	_, names, err := getDataImportAndNames(ctx, params.Unstructured)
	if err != nil {
		logger.Error(err, "failed to get DataImport")
		return err
	}

	_, err = common.DeleteDeployment(ctx, cl, cfgParams.ControllerNamespace, names.DeployName)
	if err != nil {
		return err
	}

	return nil
}

func getDataImportAndNames(ctx context.Context, unstructured *unstructured.Unstructured) (*dev1alpha1.DataImport, common.Names, error) {
	logger := log.FromContext(ctx)

	dataImport := &dev1alpha1.DataImport{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructured.UnstructuredContent(), dataImport)
	if err != nil {
		logger.Error(err, "failed to convert unstructured to DataImport")
		return nil, common.Names{}, err
	}

	// The scratch volume the populator fills is always a PVC named after the DataImport (spec.targetRef
	// now references the snapshot leaf, not the scratch PVC).
	return dataImport, common.NewNames(dev1alpha1.KindPVC, dataImport.Name, dataImport.Namespace, dataImport.Name), nil
}

func ensureDeployment(ctx context.Context, names common.Names, params populatorMachinery.PopulatorParams, dataImport *dev1alpha1.DataImport) error {
	logger := log.FromContext(ctx)

	volumeName := "pvc"

	dataImportName := types.NamespacedName{
		Namespace: dataImport.Namespace,
		Name:      dataImport.Name,
	}

	pvc := &corev1.PersistentVolumeClaim{}
	err := cl.Get(ctx, types.NamespacedName{Namespace: params.PvcPrime.Namespace, Name: params.PvcPrime.Name}, pvc)
	if err != nil {
		logger.Error(err, "Failed to get PVC")
		return err
	}

	server, err := common.MakeServerContainer(ctx, cl, common.ServerContainerCfg{
		ConfigMapName: types.NamespacedName{
			Namespace: cfgParams.ControllerNamespace,
			Name:      common.CongigMapName,
		},
		ResourceName:        dataImportName,
		VolumeName:          volumeName,
		VolumeMode:          *pvc.Spec.VolumeMode,
		Ttl:                 dataImport.Spec.Ttl,
		ServerMode:          common.ServerModeImport,
		ControllerNamespace: cfgParams.ControllerNamespace,
		Names:               names,
	})

	if err != nil {
		logger.Error(err, "Failed to make server container")
		return err
	}

	volumes := common.MakeVolumes(volumeName, params.PvcPrime.Name, false)

	podSpec := corev1.PodSpec{
		ServiceAccountName: common.ServiceAccountServer,
		ImagePullSecrets:   []corev1.LocalObjectReference{{Name: common.ImagePullSecretsName}},
		Containers:         []corev1.Container{*server},
		Volumes:            volumes,
	}

	return common.EnsureDeployment(ctx, cl, common.DeploymentCfg{
		PodSpec: podSpec,
		DeploymentName: types.NamespacedName{
			Namespace: cfgParams.ControllerNamespace,
			Name:      names.DeployName},
		ResourceName:          dataImportName,
		LabelApplicationValue: dev1alpha1.LabelDataImportValue,
	})
}
