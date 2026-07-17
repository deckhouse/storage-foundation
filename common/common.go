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
	"fmt"
	"math/rand"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

type Object interface {
	client.Object
	dev1alpha1.Statusable
}

func ContainsString(slice []string, str string) bool {
	for _, item := range slice {
		if item == str {
			return true
		}
	}
	return false
}

func RemoveString(slice []string, str string) []string {
	var result []string
	for _, item := range slice {
		if item != str {
			result = append(result, item)
		}
	}
	return result
}

func RandomString(length int) string {
	// const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"

	result := make([]byte, length)
	for i := range result {
		result[i] = charset[rand.Intn(len(charset))]
	}
	return string(result)
}

func Int32Ptr(i int32) *int32 { return &i }

// Adds a finalizer to a resource if it doesn't already exist
// Returns true if finalizer was added and needs to be persisted by the caller.
func EnsureFinalizer[T client.Object](ctx context.Context, client client.Client, resource T, finalizerName string) bool {
	logger := log.FromContext(ctx)
	logger.Info("Ensuring finalizer for resource", "resource", resource.GetName(), "finalizer", finalizerName)

	finalizers := resource.GetFinalizers()
	if !ContainsString(finalizers, finalizerName) {
		finalizers = append(finalizers, finalizerName)
		resource.SetFinalizers(finalizers)
		return true
	}
	return false
}

// Removes a finalizer from a resource if it exists
// Returns true if finalizer was removed and needs to be persisted by the caller.
func RemoveFinalizer[T client.Object](ctx context.Context, client client.Client, resource T, finalizerName string) bool {
	logger := log.FromContext(ctx)
	logger.Info("Removing finalizer from resource", "resource", resource.GetName(), "finalizer", finalizerName)

	finalizers := resource.GetFinalizers()
	if ContainsString(finalizers, finalizerName) {
		finalizers := RemoveString(finalizers, finalizerName)
		resource.SetFinalizers(finalizers)
		return true
	}
	return false
}

// ReconcileFinalizer applies the controller's single-finalizer intent (want = should the finalizer be
// present?) onto a freshly-read object's finalizer list, preserving every OTHER finalizer already on it
// (e.g. Kubernetes' foregroundDeletion). It is used by the RetryOnConflict status/metadata update path so
// re-reading the live object does not clobber concurrent third-party finalizers by blindly overwriting
// the whole list with a stale snapshot.
func ReconcileFinalizer(live []string, finalizerName string, want bool) []string {
	has := ContainsString(live, finalizerName)
	switch {
	case want && !has:
		return append(live, finalizerName)
	case !want && has:
		return RemoveString(live, finalizerName)
	default:
		return live
	}
}

// Retrieves container image from ConfigMap
func GetImage(ctx context.Context, client client.Client, name types.NamespacedName) (string, error) {
	logger := log.FromContext(ctx).WithValues("configMapNamespace", name.Namespace, "configMapName", name.Name)

	cm := &corev1.ConfigMap{}
	err := client.Get(ctx, name, cm)
	if err != nil {
		logger.Error(err, "Failed to get ConfigMap")
		return "", err
	}

	image, ok := cm.Data["image"]
	if !ok {
		err := fmt.Errorf("ConfigMap %s does not have 'image' key", name)
		logger.Error(err, "Failed to get image from ConfigMap")
		return "", err
	}

	return image, nil
}

// Get DataExport or DataImport resource based on operation type
func GetDataManager(ctx context.Context, client client.Client, name types.NamespacedName, operation Operation) (Object, error) {
	switch operation {
	case OperationExport:
		dataExport := &dev1alpha1.DataExport{}
		err := client.Get(ctx, name, dataExport)
		if err != nil {
			return nil, err
		}
		return dataExport, nil
	case OperationImport:
		dataImport := &dev1alpha1.DataImport{}
		err := client.Get(ctx, name, dataImport)
		if err != nil {
			return nil, err
		}
		return dataImport, nil
	default:
		return nil, fmt.Errorf("unknown operation: %s", operation)
	}
}

func MakeVolumes(volumeName, pvcName string, readOnly bool) []corev1.Volume {
	return []corev1.Volume{
		{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
					ReadOnly:  readOnly,
				},
			},
		},
	}
}
