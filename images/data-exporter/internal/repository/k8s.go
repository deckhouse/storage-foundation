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

package repository

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

type conditionField string

const (
	conditionExpired conditionField = "Expired"
)

type Client struct {
	client.Client
}

func NewK8sClient() (*Client, error) {
	var kubeConfig *rest.Config
	var err error

	if os.Getenv("WORKING_LOCAL") == "TRUE" {
		// Loading local config
		kubeConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")},
			&clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, err
		}
	} else {
		// Loading in-cluster config
		kubeConfig, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := dev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	k8sClient, err := client.New(kubeConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	return &Client{k8sClient}, nil
}

func (c *Client) SetDataImportCompleted(ctx context.Context, namespace, name string) error {
	// The controller writes DataImport status on every reconcile (~5s), so a plain Status().Update here
	// races with it and would fail on a stale resourceVersion. Re-GET inside RetryOnConflict so the
	// UploadFinished flip is applied onto the latest version instead of being lost.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cwtGet, cancelGet := context.WithTimeout(ctx, 10*time.Second)
		defer cancelGet()

		dataImport := &dev1alpha1.DataImport{}
		if err := c.Get(cwtGet, types.NamespacedName{Namespace: namespace, Name: name}, dataImport); err != nil {
			return err
		}

		needUpdate := common.UpdateCondition(
			ctx,
			c,
			dataImport,
			common.ConditionUploadFinished,
			metav1.ConditionTrue,
			common.ReasonUploadFinished,
			"Import completed",
		)
		if !needUpdate {
			return nil
		}

		cwtUpdate, cancelUpdate := context.WithTimeout(ctx, 10*time.Second)
		defer cancelUpdate()

		return c.Status().Update(cwtUpdate, dataImport)
	})
}

func (c *Client) CheckIsServiceAvailable(ctx context.Context, namespace, name string) (bool, error) {
	dataImport := &dev1alpha1.DataImport{}

	cwt, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	err := c.Get(cwt, types.NamespacedName{Namespace: namespace, Name: name}, dataImport)
	if err != nil {
		return false, err
	}

	condition := common.GetCondition(dataImport.Status.Conditions, common.ConditionUploadFinished)
	if condition == nil {
		return false, nil
	}

	return condition.Status == metav1.ConditionTrue, nil
}

func (c *Client) AuthenticateUserByToken(_ context.Context, token string) (authenticated bool, username string, groups []string, err error) {
	tokenReview := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token: token,
		},
	}

	err = c.Create(context.Background(), tokenReview)
	if err != nil {
		return false, "", []string{}, err
	}

	if tokenReview.Status.Authenticated {
		return true, tokenReview.Status.User.Username, tokenReview.Status.User.Groups, nil
	}

	return false, "", []string{}, err
}

func (c *Client) AuthorizeUser(_ context.Context, operation common.Operation, namespace, username string, groups []string) (allowed bool, reason string, err error) {
	var resource string
	switch operation {
	case common.OperationExport:
		resource = "dataexports"
	case common.OperationImport:
		resource = "dataimports"
	default:
		return false, "", fmt.Errorf("unknown operation: %s", operation)
	}

	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   username,
			Groups: groups,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        "create",
				Group:       "storage.deckhouse.io",
				Version:     "v1alpha1",
				Resource:    resource,
				Subresource: "download",
			},
		},
	}

	err = c.Create(context.Background(), sar)
	return sar.Status.Allowed, sar.Status.Reason, err
}

func (c *Client) UpdateAccessTimestamp(ctx context.Context, operation common.Operation, dataManagerNamespace, dataManagerName string, timestamp time.Time,
) error {
	dataManagerNamespacedName := types.NamespacedName{
		Namespace: dataManagerNamespace,
		Name:      dataManagerName,
	}

	dataManager, err := common.GetDataManager(ctx, c.Client, dataManagerNamespacedName, operation)
	if err != nil {
		return err
	}

	dataManager.GetStatus().AccessTimestamp = metav1.NewTime(timestamp)
	if err := c.Status().Update(ctx, dataManager); err != nil {
		return err
	}

	return nil
}

func (c *Client) SetStatusExpired(ctx context.Context, operation common.Operation, dataManagerNamespace, dataManagerName string) error {
	dataManagerNamespacedName := types.NamespacedName{
		Namespace: dataManagerNamespace,
		Name:      dataManagerName,
	}

	dataManager, err := common.GetDataManager(ctx, c.Client, dataManagerNamespacedName, operation)
	if err != nil {
		return err
	}

	status := dataManager.GetStatus()

	hasConditionExpired, index := hasCondition(status.Conditions, string(conditionExpired))
	if hasConditionExpired {
		if status.Conditions[index].Status != metav1.ConditionTrue {
			status.Conditions[index] = metav1.Condition{
				Type:               string(conditionExpired),
				Status:             metav1.ConditionTrue,
				Reason:             string(conditionExpired),
				Message:            "Server pod time to live expired",
				ObservedGeneration: dataManager.GetGeneration(),
				LastTransitionTime: metav1.NewTime(time.Now()),
			}
			if err := c.Status().Update(ctx, dataManager); err != nil {
				return err
			}
			return nil
		}

		return nil
	}

	return fmt.Errorf("Server pod hasn't consition Expired")
}

func hasCondition(conditions []metav1.Condition, conditionType string) (bool, int) {
	for i, condition := range conditions {
		if condition.Type == conditionType {
			return true, i
		}
	}
	return false, 0
}

func (c *Client) UpdateDataManagerCA(ctx context.Context, operation common.Operation, dataManagerNamespace, dataManagerName string, caCertPEM []byte) error {
	if len(caCertPEM) == 0 {
		return fmt.Errorf("empty CA cert")
	}
	base64CA := base64.StdEncoding.EncodeToString(caCertPEM)

	dataManagerNamespacedName := types.NamespacedName{
		Namespace: dataManagerNamespace,
		Name:      dataManagerName,
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource, err := common.GetDataManager(ctx, c.Client, dataManagerNamespacedName, operation)
		if err != nil {
			return err
		}

		status := resource.GetStatus()

		if status.CA == base64CA {
			return nil
		}

		status.CA = base64CA

		return c.Status().Update(ctx, resource)
	})
}

func (c *Client) CreateOrUpdateDataManagerCASecretIfNeeded(ctx context.Context, namespace, secretName string, caCertPEM []byte) error {
	if len(caCertPEM) == 0 {
		return fmt.Errorf("empty CA cert")
	}

	// base64CA := base64.StdEncoding.EncodeToString(caCertPEM)

	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt": caCertPEM,
		},
	}

	oldSecret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, oldSecret)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to get existing secret %s/%s: %w", namespace, secretName, err)
		}
		// Secret does not exist, create it
		return c.Create(ctx, newSecret)
	}

	// Secret exists, update it if needed
	if !bytes.Equal(oldSecret.Data["ca.crt"], caCertPEM) {
		if oldSecret.Data == nil {
			oldSecret.Data = map[string][]byte{}
		}
		oldSecret.Data["ca.crt"] = caCertPEM
		return c.Update(ctx, oldSecret)
	}

	return nil
}
