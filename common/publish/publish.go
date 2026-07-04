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

	networkingv1 "k8s.io/api/networking/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Ensures public URL available for deployment
// Shorthand to configure both headless service and ingress
func EnsurePublicURL(ctx context.Context, client client.Client, reader client.Reader, serviceCfg HeadlessServiceCfg, ingressCfg IngressCfg) (string, error) {
	logger := log.FromContext(ctx)
	logger.Info("Ensuring public URL")

	// Create or update the headless service
	if err := EnsureHeadlessService(ctx, client, serviceCfg); err != nil {
		logger.Error(err, "Failed to ensure headless service")
		return "", err
	}

	// Create or update the ingress for public access
	url, err := EnsureIngressResource(ctx, client, reader, ingressCfg)
	if err != nil {
		logger.Error(err, "Failed to ensure ingress")
		return "", err
	}

	logger.Info("Public URL is ready", "publicURL", url)

	return url, nil
}

// Deletes both the headless service and the ingress resource.
// Returns:
// - bool: true if either resource existed and was deleted, false if both resources were not found
// - error: an error if deletion of any resource fails (stops on first error)
func DeletePublicResources(ctx context.Context, client client.Client, serviceName, ingressName types.NamespacedName) (bool, error) {
	logger := log.FromContext(ctx)
	logger.Info("Deleting public resources", "serviceName", serviceName, "ingressName", ingressName)

	// Delete the headless service
	svcExists, err := DeleteHeadlessService(ctx, client, serviceName)
	if err != nil {
		logger.Error(err, "Failed to delete headless service")
		return false, err
	}

	// Delete the ingress resource
	ingressExists, err := DeleteIngressResource(ctx, client, ingressName)
	if err != nil {
		logger.Error(err, "Failed to delete ingress resource")
		return false, err
	}

	return svcExists || ingressExists, nil
}

// Checks if public URL is ready (Ingress & Headless Service are ready)
// Returns:
// - bool: true if public URL is ready, false if it is not ready
// - error: error if failed to check if public URL is ready
func IsPublishReady(ctx context.Context, client client.Client, ingressName types.NamespacedName) (bool, error) {
	logger := log.FromContext(ctx).WithValues("ingressNamespace", ingressName.Namespace, "ingressName", ingressName.Name)

	ingress := &networkingv1.Ingress{}
	err := client.Get(ctx, ingressName, ingress)
	if err != nil && !kubeerrors.IsNotFound(err) {
		logger.Error(err, "Failed to get Ingress")
		return false, err
	}

	isReady := err == nil && len(ingress.Status.LoadBalancer.Ingress) > 0
	return isReady, nil
}
