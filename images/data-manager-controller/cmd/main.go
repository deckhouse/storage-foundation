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
	"os"
	"time"

	"github.com/go-logr/stdr"
	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common/config"
	dataexport "github.com/deckhouse/storage-foundation/images/data-manager-controller/internal/controllers/data-export"
	dataimport "github.com/deckhouse/storage-foundation/images/data-manager-controller/internal/controllers/data-import"
	"github.com/deckhouse/storage-foundation/images/data-manager-controller/internal/controllers/server"
	virtv1alpha2 "github.com/deckhouse/virtualization/api/core/v1alpha2"
)

var syncPeriod = time.Hour

func main() {
	stdr.SetVerbosity(5)
	log.SetLogger(stdr.New(stdlog.New(os.Stdout, "___", stdlog.LstdFlags)))

	cfgParams := config.NewConfig()

	k8sConfig, err := loadCubeConfig()
	if err != nil {
		panic(err)
	}

	schemes := NewCombinedSchemeBuilder(
		&dev1alpha1.SchemeBuilder,
		&corev1.SchemeBuilder,
		&storagev1.SchemeBuilder,
		&appsv1.SchemeBuilder,
		&batchv1.SchemeBuilder,
		&networkingv1.SchemeBuilder,
		&snapv1.SchemeBuilder,
		&virtv1alpha2.SchemeBuilder,
		&apiextensionsv1.SchemeBuilder,
	)

	scheme := runtime.NewScheme()
	err = schemes.AddToScheme(scheme)
	if err != nil {
		log.Log.Error(err, "Failed to add schemes")
		panic(err)
	}

	manager, err := createManager(scheme, cfgParams, k8sConfig)
	if err != nil {
		panic(err)
	}

	err = createHealthzCheck(manager)
	if err != nil {
		panic(err)
	}

	err = createController(manager, cfgParams)
	if err != nil {
		panic(err)
	}

	ctx := signals.SetupSignalHandler()
	err = manager.Start(ctx)
	if err != nil {
		log.Log.Error(err, "Failed to start manager")
		panic(err)
	}
}

func loadCubeConfig() (*rest.Config, error) {
	k8sConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")},
		&clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		log.Log.Error(err, "Failed to load cube config")
		return nil, err
	}

	return k8sConfig, nil
}

func createManager(scheme *runtime.Scheme, cfgParams *config.Options, k8sConfig *rest.Config) (manager.Manager, error) {
	req, err := labels.NewRequirement(
		dev1alpha1.LabelApplicationKey,
		selection.In,
		[]string{dev1alpha1.LabelDataExportValue, dev1alpha1.LabelDataImportValue},
	)
	if err != nil {
		return nil, err
	}
	podSelector := labels.NewSelector().Add(*req)

	mgrOpts := manager.Options{
		Scheme:                        scheme,
		HealthProbeBindAddress:        cfgParams.HealthProbeBindAddress,
		LeaderElection:                cfgParams.LeaderElection,
		LeaderElectionID:              cfgParams.LeaderElectionID,
		LeaderElectionNamespace:       cfgParams.ControllerNamespace,
		LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{
			SyncPeriod: &syncPeriod,
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {
					Label: podSelector,
					Namespaces: map[string]cache.Config{
						cfgParams.ControllerNamespace: {},
					},
				},
				&corev1.Secret{}: {
					Namespaces: map[string]cache.Config{
						cfgParams.ControllerNamespace: {},
					},
				},
				&appsv1.Deployment{}: {
					Namespaces: map[string]cache.Config{
						cfgParams.ControllerNamespace: {},
					},
				},
				&networkingv1.Ingress{}: {
					Namespaces: map[string]cache.Config{
						cfgParams.ControllerNamespace: {},
					},
				},
			},
		},
	}

	manager, err := manager.New(k8sConfig, mgrOpts)
	if err != nil {
		log.Log.Error(err, "Failed to create manager")
		return nil, err
	}

	return manager, nil
}

func createHealthzCheck(manager manager.Manager) error {
	err := manager.AddHealthzCheck("healthz", healthz.Ping)
	if err != nil {
		log.Log.Error(err, "Failed to add healthz check")
		return err
	}

	err = manager.AddReadyzCheck("readyz", healthz.Ping)
	if err != nil {
		log.Log.Error(err, "Failed to add readyz check")
		return err
	}

	return nil
}

func createController(manager manager.Manager, cfgParams *config.Options) error {
	predicateForEventFilter := predicateForEventFilter()
	exportPredicate := predicatesForWatches(dev1alpha1.LabelDataExportValue)
	exportCreateRequest := createRequest(
		dev1alpha1.AnnotationStorageManagerNamespaceKey,
		dev1alpha1.AnnotationStorageManagerNameKey)

	pvPredicate := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[dev1alpha1.LabelPVDataExporter] == "true"
	})

	// Both DataExport (snapshot generic resolver, C6) and DataImport talk to dependency-free CRDs
	// (VolumeRestoreRequest/VolumeCaptureRequest/ObjectKeeper) and arbitrary snapshot leaves via a
	// dynamic client + RESTMapper, instead of compiling in domain Go types.
	dynamicClient, err := dynamic.NewForConfig(manager.GetConfig())
	if err != nil {
		log.Log.Error(err, "Failed to create dynamic client")
		return err
	}

	err = ctrl.
		NewControllerManagedBy(manager).
		For(&dev1alpha1.DataExport{}).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(exportCreateRequest),
			builder.WithPredicates(exportPredicate),
		).
		Watches(
			&networkingv1.Ingress{},
			handler.EnqueueRequestsFromMapFunc(exportCreateRequest),
			builder.WithPredicates(exportPredicate),
		).
		Watches(
			&corev1.PersistentVolume{},
			handler.EnqueueRequestsFromMapFunc(exportCreateRequest),
			builder.WithPredicates(pvPredicate),
		).
		WithEventFilter(predicateForEventFilter).
		Complete(&dataexport.DataexportReconciler{
			Client:     manager.GetClient(),
			Reader:     manager.GetAPIReader(),
			Config:     cfgParams,
			Dynamic:    dynamicClient,
			RESTMapper: manager.GetRESTMapper(),
		})
	if err != nil {
		log.Log.Error(err, "Failed to create DataExport controller")
		return err
	}

	podPredicate := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[dev1alpha1.LabelApplicationKey] == dev1alpha1.LabelDataExportValue ||
			o.GetLabels()[dev1alpha1.LabelApplicationKey] == dev1alpha1.LabelDataImportValue
	})
	err = ctrl.
		NewControllerManagedBy(manager).
		For(&corev1.Pod{}).
		WithEventFilter(podPredicate).
		Complete(&server.PodReconciler{
			Client: manager.GetClient(),
			Reader: manager.GetAPIReader(),
			Config: cfgParams,
		})
	if err != nil {
		log.Log.Error(err, "Failed to create DataExport pod controller")
		return err
	}

	importPredicate := predicatesForWatches(dev1alpha1.LabelDataImportValue)
	importCreateRequest := createRequest(
		dev1alpha1.AnnotationStorageManagerNamespaceKey,
		dev1alpha1.AnnotationStorageManagerNameKey)

	// The DataImport reconciler reuses the dynamic client (VolumeCaptureRequest, ObjectKeeper). Volume
	// parameters now come from the DataImport spec, so it no longer reads the leaf's captured manifests.
	err = ctrl.
		NewControllerManagedBy(manager).
		For(&dev1alpha1.DataImport{}).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(importCreateRequest),
			builder.WithPredicates(importPredicate),
		).
		Watches(
			&batchv1.Job{},
			handler.EnqueueRequestsFromMapFunc(importCreateRequest),
			builder.WithPredicates(importPredicate),
		).
		Watches(
			&networkingv1.Ingress{},
			handler.EnqueueRequestsFromMapFunc(importCreateRequest),
			builder.WithPredicates(importPredicate),
		).
		Watches(
			&corev1.PersistentVolumeClaim{},
			handler.EnqueueRequestsFromMapFunc(importCreateRequest),
			builder.WithPredicates(importPredicate),
		).
		WithEventFilter(predicateForEventFilter).
		Complete(&dataimport.DataImportReconciler{
			Client:  manager.GetClient(),
			Reader:  manager.GetAPIReader(),
			Config:  cfgParams,
			Dynamic: dynamicClient,
		})
	if err != nil {
		return err
	}

	return nil
}

func predicateForEventFilter() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(_ client.Object) bool {
		return true
	})
}

func predicatesForWatches(appKey string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[dev1alpha1.LabelApplicationKey] == appKey
	})
}

func createRequest(namespaceKey string, nameKey string) func(_ context.Context, obj client.Object) []reconcile.Request {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		annotations := obj.GetAnnotations()
		if len(annotations) == 0 {
			return nil
		}
		ns := annotations[namespaceKey]
		name := annotations[nameKey]
		if ns == "" || name == "" {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
		}}
	}
}
