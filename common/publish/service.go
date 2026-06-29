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

package publish

import (
	"context"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

type HeadlessServiceCfg struct {
	ServiceName           types.NamespacedName // Headless service name
	DeploymentName        string               // Deployment name to match in selector
	LabelApplicationValue string               // DataImport or DataExport label value
}

// Ensures headless service is created or updated
func EnsureHeadlessService(ctx context.Context, cl client.Client, cfg HeadlessServiceCfg) error {
	logger := log.FromContext(ctx).WithValues("serviceNamespace", cfg.ServiceName.Namespace, "serviceName", cfg.ServiceName.Name)
	logger.Info("Ensuring headless service")

	newSvc := makeHeadlessService(cfg)
	oldSvc, err := GetHeadlessService(ctx, cl, cfg.ServiceName)
	if err != nil {
		return err
	}

	if oldSvc == nil {
		logger.Info("Creating new headless service")
		if err := cl.Create(ctx, newSvc); err != nil {
			logger.Error(err, "Failed to create new headless service")
			return err
		}
		logger.Info("Created new headless service")
		return nil
	}

	return handleExistingHeadlessService(ctx, cl, oldSvc, newSvc)
}

// Returns headless service by name
func GetHeadlessService(ctx context.Context, cl client.Client, name types.NamespacedName) (*corev1.Service, error) {
	logger := log.FromContext(ctx).WithValues("serviceNamespace", name.Namespace, "serviceName", name.Name)

	svc := &corev1.Service{}
	if err := cl.Get(ctx, name, svc); err != nil {
		if !kubeerrors.IsNotFound(err) {
			logger.Error(err, "Failed to get headless service")
			return nil, err
		}
		return nil, nil
	}
	return svc, nil
}

// Deletes a headless Service by name
// Returns:
// - bool: true if headless service was deleted, false if it was not found
// - error: error if failed to delete headless service
func DeleteHeadlessService(ctx context.Context, cl client.Client, name types.NamespacedName) (bool, error) {
	logger := log.FromContext(ctx).WithValues("serviceNamespace", name.Namespace, "serviceName", name.Name)

	logger.Info("Deleting headless service")

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: name.Namespace,
			Name:      name.Name,
		},
	}

	err := cl.Delete(ctx, svc)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			logger.Info("Headless service not found")
			return false, nil
		}

		logger.Error(err, "Failed to delete headless service")
		return false, err
	}

	logger.Info("Headless service deleted")
	return true, nil
}

func makeHeadlessService(cfg HeadlessServiceCfg) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.ServiceName.Name,
			Namespace: cfg.ServiceName.Namespace,
			Labels: map[string]string{
				dev1alpha1.LabelApplicationKey: cfg.LabelApplicationValue,
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:  corev1.ClusterIPNone,                   // Make it a headless service
			ClusterIPs: []string{string(corev1.ClusterIPNone)}, // Make it a headless service
			Selector: map[string]string{
				dev1alpha1.LabelApplicationKey:                  cfg.LabelApplicationValue,
				dev1alpha1.LabelStorageManagerDeploymentNameKey: cfg.DeploymentName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     common.FileServerPort,
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}
}

func handleExistingHeadlessService(ctx context.Context, client client.Client, oldSvc *corev1.Service, newSvc *corev1.Service) error {
	logger := log.FromContext(ctx).WithValues("serviceNamespace", oldSvc.Namespace, "serviceName", oldSvc.Name)
	if needHeadlessServiceUpdate(oldSvc, newSvc) {
		logger.Info("Updating headless service")
		oldSvc.Spec = newSvc.Spec
		oldSvc.Labels = newSvc.Labels
		if err := client.Update(ctx, oldSvc); err != nil {
			logger.Error(err, "Failed to update headless service")
			return err
		}
		logger.Info("Successfully updated headless service")
		return nil
	}

	logger.Info("Headless service is up to date")
	return nil
}

func needHeadlessServiceUpdate(oldSvc *corev1.Service, newSvc *corev1.Service) bool {
	labelsEqual := reflect.DeepEqual(oldSvc.Labels, newSvc.Labels)
	selectorEqual := reflect.DeepEqual(oldSvc.Spec.Selector, newSvc.Spec.Selector)
	portsEqual := reflect.DeepEqual(oldSvc.Spec.Ports, newSvc.Spec.Ports)
	return !(labelsEqual && selectorEqual && portsEqual)
}
