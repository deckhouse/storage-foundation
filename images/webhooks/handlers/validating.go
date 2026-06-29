/*
Copyright 2026 Flant JSC

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

package handlers

import (
	"context"
	"net/http"

	kwhhttp "github.com/slok/kubewebhook/v2/pkg/http"
	"github.com/slok/kubewebhook/v2/pkg/log"
	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type validateFunc func(ctx context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error)

// NewValidatingWebhookHandler wires a plain validation function into a
// kubewebhook HTTP handler. obj is the typed zero-value used to decode the
// incoming admission request (e.g. &DataExport{}).
func NewValidatingWebhookHandler(validationFunc validateFunc, validatorID string, obj metav1.Object, logger log.Logger) (http.Handler, error) {
	validatingWebhook, err := kwhvalidating.NewWebhook(kwhvalidating.WebhookConfig{
		ID:        validatorID,
		Obj:       obj,
		Validator: kwhvalidating.ValidatorFunc(validationFunc),
		Logger:    logger,
	})
	if err != nil {
		return nil, err
	}

	return kwhhttp.HandlerFor(kwhhttp.HandlerConfig{Webhook: validatingWebhook, Logger: logger})
}
