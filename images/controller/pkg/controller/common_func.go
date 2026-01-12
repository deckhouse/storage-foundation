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

package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func shouldReconcileByDeleteFunc(obj metav1.Object) bool {
	return obj.GetDeletionTimestamp() != nil
}

func removeFinalizerIfExists(ctx context.Context, cl client.Client, obj metav1.Object, finalizerName string) error {
	removed := false
	finalizers := obj.GetFinalizers()
	for i, f := range finalizers {
		if f == finalizerName {
			finalizers = append(finalizers[:i], finalizers[i+1:]...)
			removed = true
			break
		}
	}

	if removed {
		obj.SetFinalizers(finalizers)
		err := cl.Update(ctx, obj.(client.Object))
		if err != nil {
			return err
		}
	}

	return nil
}

func addFinalizerIfNotExists(ctx context.Context, cl client.Client, obj metav1.Object, finalizerName string) (bool, error) {
	added := false
	finalizers := obj.GetFinalizers()
	if !slices.Contains(finalizers, finalizerName) {
		finalizers = append(finalizers, finalizerName)
		added = true
	}

	if added {
		obj.SetFinalizers(finalizers)
		err := cl.Update(ctx, obj.(client.Object))
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func CombineBase64(encoded1, encoded2 string) (string, error) {
	var data1, data2 []byte
	var err error

	if encoded1 == "" {
		return encoded2, nil
	}

	if encoded2 == "" {
		return encoded1, nil
	}

	data1, err = base64.StdEncoding.DecodeString(encoded1)
	if err != nil {
		return "", fmt.Errorf("failed to decode the first input: %w", err)
	}

	data2, err = base64.StdEncoding.DecodeString(encoded2)
	if err != nil {
		return "", fmt.Errorf("failed to decode the second input: %w", err)
	}

	data1 = append(data1, data2...)

	combinedEncoded := base64.StdEncoding.EncodeToString(data1)

	return combinedEncoded, nil
}
