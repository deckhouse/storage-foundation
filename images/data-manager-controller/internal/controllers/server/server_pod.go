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

package server

import (
	"context"
	"fmt"
	"log"

	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

type PodReconciler struct {
	Client client.Client
	Reader client.Reader
	Config *config.Options
}

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace == r.Config.ControllerNamespace {
		pod := &corev1.Pod{}
		err := r.Client.Get(ctx, req.NamespacedName, pod)
		if err != nil {
			if kubeerrors.IsNotFound(err) {
				// Pod not found, it may have been deleted after the event was received.
				log.Printf("Pod %s/%s not found, it may have been deleted\n", req.Namespace, req.Name)
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to get pod from cache: %w", err)
		}

		// find Dataexporter resource owner and update pod URL
		if pod.Status.Phase == corev1.PodRunning {
			var dataExportNamespacedName types.NamespacedName
			var ok bool
			if dataExportNamespacedName.Namespace, ok = pod.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey]; !ok {
				return ctrl.Result{}, nil
			}
			if dataExportNamespacedName.Name, ok = pod.Annotations[dev1alpha1.AnnotationStorageManagerNameKey]; !ok {
				return ctrl.Result{}, nil
			}
			log.Printf("Found running DataManager pod: %s/%s\n", req.Namespace, req.Name)

			operation, err := getOperationFromPod(pod)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get operation mode: %w", err)
			}

			dataManager, err := common.GetDataManager(ctx, r.Client, dataExportNamespacedName, operation)
			if err != nil {
				if kubeerrors.IsNotFound(err) {
					// The owning DataExport/DataImport is already gone (deleted or garbage-collected) while
					// its pod is still terminating. There is nothing to update; the pod is reaped by the
					// owner's finalizer cleanup, not by this reconciler. Log and stop without requeue so a
					// short-lived orphan pod does not produce an error-requeue storm.
					log.Printf("Owner DataManager %s/%s not found for pod %s/%s, skipping URL update\n",
						dataExportNamespacedName.Namespace, dataExportNamespacedName.Name, req.Namespace, req.Name)
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, fmt.Errorf("failed to get DataManager resource from cache: %w", err)
			}

			url := fmt.Sprintf("https://%s:%d/", pod.Status.PodIP, common.FileServerPort)
			status := dataManager.GetStatus()
			if status.Url != url {
				status.Url = url
				err = r.Client.Status().Update(ctx, dataManager)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("failed update URL for DataManager pod for DataManager resource %s, %s: %w", dataExportNamespacedName.Namespace, dataExportNamespacedName.Name, err)
				}
				log.Printf("URL for DataManager updated: %s/%s\n", req.Namespace, req.Name)
			}

			return ctrl.Result{}, nil
		}
	}

	return ctrl.Result{}, nil
}

func getOperationFromPod(pod *corev1.Pod) (common.Operation, error) {
	annotations := pod.GetLabels()
	operation := annotations[dev1alpha1.LabelApplicationKey]
	switch operation {
	case dev1alpha1.LabelDataExportValue:
		return common.OperationExport, nil
	case dev1alpha1.LabelDataImportValue:
		return common.OperationImport, nil
	default:
		return "", fmt.Errorf("unknown operation: %s", operation)
	}
}
