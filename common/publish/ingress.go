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
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/storage-foundation/common"
)

const (
	KubeAPISubdomain = "api"
	ConsoleSubdomain = "console"

	// Nginx Ingress annotations
	AnnotationBackendProtocol  = "nginx.ingress.kubernetes.io/backend-protocol"
	AnnotationEnableCORS       = "nginx.ingress.kubernetes.io/enable-cors"
	AnnotationCORSAllowMethods = "nginx.ingress.kubernetes.io/cors-allow-methods"
	AnnotationCORSAllowOrigin  = "nginx.ingress.kubernetes.io/cors-allow-origin"
	AnnotationProxyBodySize    = "nginx.ingress.kubernetes.io/proxy-body-size"
)

type IngressCfg struct {
	IngressName      types.NamespacedName // Where to create/update the Ingress
	ServiceName      types.NamespacedName // Headless Service to route traffic to (same namespace as Ingress)
	OriginIngress    types.NamespacedName // Source Ingress (TLS secret provider)
	TargetSecretName string               // Secret name (in Ingress namespace) to create/update for TLS
	Path             string               // Public URL path
	CorsAllowMethods string               // CORS allowed methods (e.g., "GET, HEAD, OPTIONS")
	// ProxyBodySize, when non-empty, sets nginx.ingress.kubernetes.io/proxy-body-size (nginx
	// client_max_body_size) on the Ingress. Empty leaves the controller default (1m), which suits
	// download-only ingresses (DataExport). Upload ingresses (DataImport PUT) must raise it, otherwise
	// nginx rejects a body larger than 1m with 413 before the request reaches the exporter.
	ProxyBodySize string
}

// Ensures Ingress resource is created or updated
// Returns:
// - public URL of the Ingress resource
func EnsureIngressResource(ctx context.Context, cl client.Client, rd client.Reader, cfg IngressCfg) (string, error) {
	logger := log.FromContext(ctx).WithValues("ingressNamespace", cfg.IngressName.Namespace, "ingressName", cfg.IngressName.Name)
	logger.Info("Ensuring Ingress resource")

	publicDomain, exists := os.LookupEnv("ingressPublicDomain")
	if !exists {
		return "", fmt.Errorf("environment variable ingressPublicDomain not defined")
	}

	ingressClassName, ok := os.LookupEnv("ingressClassName")
	if !ok {
		return "", fmt.Errorf("environment variable ingressClassName not defined")
	}

	publicDomain = strings.TrimPrefix(publicDomain, "%s.")
	host := fmt.Sprintf("%s.%s", KubeAPISubdomain, publicDomain)
	publicURL := fmt.Sprintf("https://%s%s/", host, cfg.Path)
	pathType := networkingv1.PathTypeImplementationSpecific

	originSecretName, err := getIngressSecretNameFromIngress(ctx, rd, cfg.OriginIngress, host)
	if err != nil {
		return "", fmt.Errorf("failed to get ingress secret name from origin ingress: %w", err)
	}

	logger.Info("Origin secret name", "originIngressNamespace", cfg.OriginIngress.Namespace, "originIngressName", cfg.OriginIngress.Name, "originSecretName", originSecretName)
	if err := ensureIngressSecret(ctx, cl, rd, cfg.OriginIngress.Namespace, originSecretName, cfg.IngressName.Namespace, cfg.TargetSecretName); err != nil {
		return "", fmt.Errorf("failed to create or update ingress secret: %w", err)
	}

	newIngress := makeIngress(cfg, host, cfg.Path, &pathType, ingressClassName, publicDomain)
	oldIngress, err := GetIngress(ctx, cl, cfg.IngressName)
	if err != nil {
		return "", err
	}

	if oldIngress == nil {
		logger.Info("Creating new Ingress resource")
		if err := cl.Create(ctx, newIngress); err != nil {
			logger.Error(err, "Failed to create new Ingress resource")
			return "", err
		}
		logger.Info("Created new Ingress resource")
	} else {
		if err := handleExistingIngress(ctx, cl, oldIngress, newIngress); err != nil {
			return "", err
		}
	}

	// wait for Ingress resource to be ready
	if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		ingress, err := GetIngress(ctx, cl, cfg.IngressName)
		if err != nil {
			if kubeerrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("failed to get Ingress resource %s/%s: %w", cfg.IngressName.Namespace, cfg.IngressName.Name, err)
		}

		if len(ingress.Status.LoadBalancer.Ingress) > 0 {
			return true, nil
		}
		return false, nil
	}); err != nil {
		return "", fmt.Errorf("failed to wait for Ingress resource %s/%s to be ready: %w", cfg.IngressName.Namespace, cfg.IngressName.Name, err)
	}

	return publicURL, nil
}

// Returns Ingress resource by name
func GetIngress(ctx context.Context, cl client.Client, name types.NamespacedName) (*networkingv1.Ingress, error) {
	logger := log.FromContext(ctx).WithValues("ingressNamespace", name.Namespace, "ingressName", name.Name)
	ing := &networkingv1.Ingress{}
	if err := cl.Get(ctx, name, ing); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil
		}
		logger.Error(err, "Failed to get Ingress resource")
		return nil, err
	}
	return ing, nil
}

// Deletes Ingress resource if exists
// Returns:
// - bool: true if Ingress resource was deleted, false if it was not found
// - error: error if failed to delete Ingress resource
func DeleteIngressResource(ctx context.Context, cl client.Client, name types.NamespacedName) (bool, error) {
	logger := log.FromContext(ctx).WithValues("ingressNamespace", name.Namespace, "ingressName", name.Name)

	logger.Info("Deleting Ingress resource")

	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: name.Namespace,
			Name:      name.Name,
		},
	}

	err := cl.Delete(ctx, ing)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			logger.Info("Ingress resource not found")
			return false, nil
		}

		logger.Error(err, "Failed to delete Ingress resource")
		return false, err
	}
	logger.Info("Ingress resource deleted")
	return true, nil
}

func makeIngress(cfg IngressCfg, host, path string, pathType *networkingv1.PathType, ingressClassName string, publicDomain string) *networkingv1.Ingress {
	annotations := map[string]string{
		AnnotationBackendProtocol: "HTTPS",
	}

	if cfg.CorsAllowMethods != "" {
		corsOrigin := fmt.Sprintf("https://%s.%s", ConsoleSubdomain, publicDomain)
		annotations[AnnotationEnableCORS] = "true"
		annotations[AnnotationCORSAllowMethods] = cfg.CorsAllowMethods
		annotations[AnnotationCORSAllowOrigin] = corsOrigin
	}

	if cfg.ProxyBodySize != "" {
		annotations[AnnotationProxyBodySize] = cfg.ProxyBodySize
	}

	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        cfg.IngressName.Name,
			Namespace:   cfg.IngressName.Namespace,
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     path,
									PathType: pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: cfg.ServiceName.Name,
											Port: networkingv1.ServiceBackendPort{Number: common.FileServerPort},
										},
									},
								},
							},
						},
					},
				},
			},
			TLS:              []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: cfg.TargetSecretName}},
			IngressClassName: &ingressClassName,
		},
	}
}

func handleExistingIngress(ctx context.Context, client client.Client, oldIng *networkingv1.Ingress, newIng *networkingv1.Ingress) error {
	logger := log.FromContext(ctx).WithValues("ingressNamespace", oldIng.Namespace, "ingressName", oldIng.Name)
	if needIngressUpdate(oldIng, newIng) {
		logger.Info("Updating Ingress resource")
		oldIng.Spec = newIng.Spec
		oldIng.Annotations = newIng.Annotations
		if err := client.Update(ctx, oldIng); err != nil {
			logger.Error(err, "Failed to update Ingress resource")
			return err
		}
		logger.Info("Successfully updated Ingress resource")
		return nil
	}
	logger.Info("Ingress resource is up to date")
	return nil
}

func needIngressUpdate(oldIng *networkingv1.Ingress, newIng *networkingv1.Ingress) bool {
	annoEqual := reflect.DeepEqual(oldIng.Annotations, newIng.Annotations)
	rulesEqual := reflect.DeepEqual(oldIng.Spec.Rules, newIng.Spec.Rules)
	tlsEqual := reflect.DeepEqual(oldIng.Spec.TLS, newIng.Spec.TLS)
	classEqual := (oldIng.Spec.IngressClassName == nil && newIng.Spec.IngressClassName == nil) ||
		(oldIng.Spec.IngressClassName != nil && newIng.Spec.IngressClassName != nil && *oldIng.Spec.IngressClassName == *newIng.Spec.IngressClassName)
	return !(annoEqual && rulesEqual && tlsEqual && classEqual)
}

func getIngressSecretNameFromIngress(ctx context.Context, rd client.Reader, origin types.NamespacedName, host string) (string, error) {
	ing := &networkingv1.Ingress{}
	if err := rd.Get(ctx, origin, ing); err != nil {
		return "", fmt.Errorf("failed to get Ingress resource %s/%s: %w", origin.Namespace, origin.Name, err)
	}
	if len(ing.Spec.TLS) == 0 {
		return "", fmt.Errorf("ingress resource %s/%s does not have TLS configured", origin.Namespace, origin.Name)
	}
	for _, tls := range ing.Spec.TLS {
		for _, h := range tls.Hosts {
			if h == host {
				if tls.SecretName == "" {
					return "", fmt.Errorf("ingress resource %s/%s has empty SecretName for host %s", origin.Namespace, origin.Name, host)
				}
				return tls.SecretName, nil
			}
		}
	}
	return "", fmt.Errorf("no TLS configuration found for host %s in Ingress resource %s/%s", host, origin.Namespace, origin.Name)
}

func ensureIngressSecret(ctx context.Context, client client.Client, reader client.Reader, originSecretNamespace, originSecretName, targetNamespace, targetSecretName string) error {
	logger := log.FromContext(ctx).WithValues("originSecretNamespace", originSecretNamespace, "originSecretName", originSecretName, "targetNamespace", targetNamespace, "targetSecretName", targetSecretName)
	origin := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: originSecretNamespace, Name: originSecretName}, origin); err != nil {
		logger.Error(err, "Failed to get origin secret")
		return err
	}
	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      targetSecretName,
			Namespace: targetNamespace,
			Labels: map[string]string{
				"app": "data-exporter",
			},
		},
		Data: origin.Data,
	}
	old := &corev1.Secret{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: targetNamespace, Name: targetSecretName}, old); err != nil {
		if kubeerrors.IsNotFound(err) {
			return client.Create(ctx, newSecret)
		}
		logger.Error(err, "Failed to get secret")
		return err
	}
	if !reflect.DeepEqual(old.Data, newSecret.Data) {
		old.Data = newSecret.Data
		return client.Update(ctx, old)
	}
	return nil
}
