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

package common

import (
	"context"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

type ServerMode string

const (
	ServerModeExport ServerMode = "export"
	ServerModeImport ServerMode = "import"
)

type DeploymentCfg struct {
	PodSpec               corev1.PodSpec
	DeploymentName        types.NamespacedName
	ResourceName          types.NamespacedName // DataImport or DataExport resource name
	LabelApplicationValue string               // DataImport or DataExport label value
}

// const ContainerVolumeName = "pvc"

// EnsureDeployment creates or updates a Deployment based on the configuration
func EnsureDeployment(ctx context.Context, client client.Client, cfg DeploymentCfg) error {
	logger := log.FromContext(ctx).WithValues("deploymentName", cfg.DeploymentName)
	logger.Info("Ensuring Deployment")

	newDeploy := makeDeployment(cfg)
	oldDeploy := &appsv1.Deployment{}
	err := client.Get(ctx, cfg.DeploymentName, oldDeploy)
	if err != nil && !kubeerrors.IsNotFound(err) {
		logger.Error(err, "Failed to get Deployment")
		return err
	}

	if kubeerrors.IsNotFound(err) {
		// Deployment does not exist, create it
		logger.Info("Creating new Deployment")
		err = client.Create(ctx, newDeploy)
		if err != nil {
			logger.Error(err, "Failed to create new Deployment")
			return err
		}

		logger.Info("Created new Deployment")
		return nil
	}

	return handleExistingDeployment(ctx, client, oldDeploy, newDeploy)
}

// DeleteDeployment deletes a Deployment by namespace and name
// Returns:
// - bool: true if Deployment was deleted, false if it was not found
// - error: error if failed to delete Deployment
func DeleteDeployment(ctx context.Context, client client.Client, namespace, name string) (bool, error) {
	logger := log.FromContext(ctx).WithValues("deploymentName", name)
	logger.Info("Deleting Deployment")

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}

	err := client.Delete(ctx, deploy)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			logger.Info("Deployment not found")
			return false, nil
		}

		logger.Error(err, "Failed to delete Deployment")
		return false, err
	}
	logger.Info("Deployment deleted")
	return true, nil
}

// Checks if Deployment is ready
// Returns:
// - bool: true if Deployment is ready, false if it is not ready
// - error: error if failed to check if Deployment is ready
func IsDeploymentReady(ctx context.Context, client client.Client, deploymentName types.NamespacedName) (bool, error) {
	logger := log.FromContext(ctx).WithValues("deploymentNamespace", deploymentName.Namespace, "deploymentName", deploymentName.Name)

	deployment := &appsv1.Deployment{}
	err := client.Get(ctx, deploymentName, deployment)
	if err != nil && !kubeerrors.IsNotFound(err) {
		logger.Error(err, "Failed to get Deployment")
		return false, err
	}

	isReady := err == nil && deployment.Status.ReadyReplicas == *deployment.Spec.Replicas
	return isReady, nil
}

func makeDeployment(cfg DeploymentCfg) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.DeploymentName.Name,
			Namespace: cfg.DeploymentName.Namespace,
			Labels: map[string]string{
				dev1alpha1.LabelApplicationKey: cfg.LabelApplicationValue, // To watch resource by controller
			},
			Annotations: map[string]string{
				// To identify which import/export resource is managing this deployment
				dev1alpha1.AnnotationStorageManagerNamespaceKey: cfg.ResourceName.Namespace,
				dev1alpha1.AnnotationStorageManagerNameKey:      cfg.ResourceName.Name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: Int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					dev1alpha1.LabelApplicationKey:                  cfg.LabelApplicationValue,
					dev1alpha1.LabelStorageManagerDeploymentNameKey: cfg.DeploymentName.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						dev1alpha1.LabelApplicationKey:                  cfg.LabelApplicationValue, // To watch resource by controller
						dev1alpha1.LabelStorageManagerDeploymentNameKey: cfg.DeploymentName.Name,
					},
					Annotations: map[string]string{
						// To identify which import/export resource is managing this deployment
						dev1alpha1.AnnotationStorageManagerNamespaceKey: cfg.ResourceName.Namespace,
						dev1alpha1.AnnotationStorageManagerNameKey:      cfg.ResourceName.Name,
					},
				},
				Spec: cfg.PodSpec,
			},
		},
	}
}

func handleExistingDeployment(ctx context.Context, client client.Client, oldDeploy *appsv1.Deployment, newDeploy *appsv1.Deployment) error {
	logger := log.FromContext(ctx).WithValues("deploymentNamespace", oldDeploy.Namespace, "deploymentName", oldDeploy.Name)
	logger.Info("Handling existing Deployment")

	if needDeploymentUpdate(oldDeploy, newDeploy) {
		logger.Info("Updating Deployment")

		// TODO: we update only spec. What if meta changed (labels, annotations etc)?
		oldDeploy.Spec = newDeploy.Spec

		err := client.Update(ctx, oldDeploy)
		if err != nil {
			logger.Error(err, "Failed to update Deployment")
			return err
		}
		logger.Info("Successfully updated Deployment")
		return nil
	} else {
		logger.Info("Deployment is up to date")
		return nil
	}
}

func needDeploymentUpdate(oldDeploy *appsv1.Deployment, newDeploy *appsv1.Deployment) bool {
	oldSpec := &oldDeploy.Spec
	newSpec := &newDeploy.Spec

	// TODO: clarify which fields we should compare
	specEqual := (oldSpec.Replicas != nil && newSpec.Replicas != nil) &&
		*oldSpec.Replicas == *newSpec.Replicas &&
		reflect.DeepEqual(oldSpec.Template.Labels, newSpec.Template.Labels) &&
		reflect.DeepEqual(oldSpec.Selector.MatchLabels, newSpec.Selector.MatchLabels) &&
		oldSpec.Template.Spec.ServiceAccountName == newSpec.Template.Spec.ServiceAccountName &&
		reflect.DeepEqual(oldSpec.Template.Spec.ImagePullSecrets, newSpec.Template.Spec.ImagePullSecrets) &&
		len(oldSpec.Template.Spec.Containers) == 1 && len(newSpec.Template.Spec.Containers) == 1 &&
		oldSpec.Template.Spec.Containers[0].Image == newSpec.Template.Spec.Containers[0].Image &&
		reflect.DeepEqual(oldSpec.Template.Spec.Containers[0].Args, newSpec.Template.Spec.Containers[0].Args) &&
		len(oldSpec.Template.Spec.Containers[0].Ports) == 1 && len(newSpec.Template.Spec.Containers[0].Ports) == 1 &&
		oldSpec.Template.Spec.Containers[0].Ports[0].ContainerPort == newSpec.Template.Spec.Containers[0].Ports[0].ContainerPort &&
		reflect.DeepEqual(oldSpec.Template.Spec.Volumes, newSpec.Template.Spec.Volumes)

	return !specEqual
}
