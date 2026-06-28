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

package dataimport

import (
	"context"
	"fmt"
	"reflect"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// Creates or updates a PVC based on the template
func EnsurePVC(
	ctx context.Context,
	client client.Client,
	resourceName types.NamespacedName,
	pvcTemplate *dev1alpha1.PersistentVolumeClaimTemplateSpec,
	dataSourceRef *corev1.TypedObjectReference,
) error {
	logger := log.FromContext(ctx).WithValues("pvcName", pvcTemplate.Name)
	logger.Info("Ensuring PVC")

	newPVC := makePVC(pvcTemplate, dataSourceRef, resourceName)

	namespacedName := types.NamespacedName{
		Namespace: resourceName.Namespace, // PVC is created in same namespace as DataImport resource
		Name:      pvcTemplate.Name,
	}

	oldPVC := &corev1.PersistentVolumeClaim{}
	err := client.Get(ctx, namespacedName, oldPVC)
	if err != nil && !kubeerrors.IsNotFound(err) {
		logger.Error(err, "Failed to get PVC")
		return err
	}

	if kubeerrors.IsNotFound(err) {
		// PVC doesn't exist, create new PVC
		logger.Info("Creating new PVC")

		err := client.Create(ctx, newPVC)
		if err != nil {
			logger.Error(err, "Failed to create PVC")
			return err
		}

		logger.Info("Successfully created PVC")
		return nil
	}

	// PVC exists, check its state and handle accordingly
	return handleExistingPVC(ctx, client, oldPVC, newPVC)
}

func makePVC(
	pvcTemplate *dev1alpha1.PersistentVolumeClaimTemplateSpec,
	dataSourceRef *corev1.TypedObjectReference,
	resourceName types.NamespacedName,
) *corev1.PersistentVolumeClaim {
	modes := lo.Map(
		pvcTemplate.AccessModes,
		func(mode dev1alpha1.PersistentVolumeAccessMode, _ int) corev1.PersistentVolumeAccessMode {
			return corev1.PersistentVolumeAccessMode(mode)
		})

	requests := lo.MapEntries(
		pvcTemplate.Resources.Requests,
		func(name dev1alpha1.ResourceName, quantity resource.Quantity) (corev1.ResourceName, resource.Quantity) {
			return corev1.ResourceName(name), quantity
		})

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcTemplate.Name,
			Namespace: resourceName.Namespace, // PVC is created in same namespace as DataImport resource
			Labels: lo.Assign(pvcTemplate.Labels, map[string]string{
				dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataImportValue,
			}),
			Annotations: lo.Assign(pvcTemplate.Annotations, map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: resourceName.Namespace,
				dev1alpha1.AnnotationStorageManagerNameKey:      resourceName.Name,
			}),
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: modes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: requests,
			},
			StorageClassName: pvcTemplate.StorageClassName,
			VolumeMode:       (*corev1.PersistentVolumeMode)(pvcTemplate.VolumeMode),
			DataSourceRef:    dataSourceRef,
		},
	}

	return pvc
}

func handleExistingPVC(ctx context.Context, client client.Client, existingPVC *corev1.PersistentVolumeClaim, newPVC *corev1.PersistentVolumeClaim) error {
	logger := log.FromContext(ctx).WithValues("pvcNamespace", existingPVC.Namespace, "pvcName", existingPVC.Name)
	logger.Info("Handling existing PVC")

	// Check if PVC spec needs to be updated
	if needsPVCSpecUpdate(existingPVC, newPVC) {
		logger.Info("Updating PVC")

		// TODO: we update only spec. What if meta changed (labels, annotations etc)?
		existingPVC.Spec = newPVC.Spec

		err := client.Update(ctx, existingPVC)
		if err != nil {
			logger.Error(err, "Failed to update PVC")
			return err
		}
		logger.Info("Successfully updated PVC")
		return nil
	}
	logger.Info("PVC is up to date")
	return nil
}

func needsPVCSpecUpdate(existingPVC *corev1.PersistentVolumeClaim, newPVC *corev1.PersistentVolumeClaim) bool {
	return !reflect.DeepEqual(existingPVC.Spec.AccessModes, newPVC.Spec.AccessModes) ||
		!reflect.DeepEqual(existingPVC.Spec.Resources.Requests, newPVC.Spec.Resources.Requests) ||
		!ptrEqual(existingPVC.Spec.StorageClassName, newPVC.Spec.StorageClassName) ||
		!ptrEqual(existingPVC.Spec.VolumeMode, newPVC.Spec.VolumeMode)
}

func ptrEqual[T comparable](a, b *T) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func CheckPVCStatus(
	ctx context.Context,
	client client.Client,
	pvc *corev1.PersistentVolumeClaim,
	waitForFirstConsumer bool,
) (TargetStatus, error) {
	logger := log.FromContext(ctx).WithValues("pvcNamespace", pvc.Namespace, "pvcName", pvc.Name)

	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		return TargetStatusReady, nil
	case corev1.ClaimPending:
		scWffc, err := isWaitForFirstConsumer(ctx, client, pvc)
		if err != nil {
			logger.Error(err, "Failed to check if need dummy pod")
			return TargetStatusUnknown, err
		}

		if scWffc && !waitForFirstConsumer {
			return TargetStatusNeedConsumer, nil
		}
		return TargetStatusPending, nil
	default:
		return TargetStatusFailed, nil
	}
}

func isWaitForFirstConsumer(ctx context.Context, client client.Client, pvc *corev1.PersistentVolumeClaim) (bool, error) {
	logger := log.FromContext(ctx).WithValues("pvcNamespace", pvc.Namespace, "pvcName", pvc.Name)

	storageClassName, err := getStorageClass(pvc)
	if err != nil {
		logger.Error(err, "Failed to get storage class")
		return false, err
	}

	storageClass := &storagev1.StorageClass{}
	err = client.Get(ctx, types.NamespacedName{Name: storageClassName}, storageClass)
	if err != nil {
		logger.Error(err, "Failed to get storage class")
		return false, err
	}

	// Check if the volume binding mode is WaitForFirstConsumer
	if storageClass.VolumeBindingMode != nil && *storageClass.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer {
		return true, nil
	}

	return storageClass.VolumeBindingMode != nil && *storageClass.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer, nil
}

func getStorageClass(pvc *corev1.PersistentVolumeClaim) (string, error) {
	if pvc.Spec.StorageClassName == nil {
		return "", fmt.Errorf("PVC %s/%s does not have a storage class specified", pvc.Namespace, pvc.Name)
	}
	return *pvc.Spec.StorageClassName, nil
}

// GetPVCVolumeMode returns the volume mode of a PVC
func GetPVCVolumeMode(pvc *corev1.PersistentVolumeClaim) (string, error) {
	if pvc.Spec.VolumeMode == nil {
		return "", fmt.Errorf("PVC %s/%s does not have a volume mode specified", pvc.Namespace, pvc.Name)
	}
	return string(*pvc.Spec.VolumeMode), nil
}

// RemovePVCFinalizer removes a finalizer from a PVC by namespace and name
func RemovePVCFinalizer(ctx context.Context, client client.Client, namespacedName types.NamespacedName, finalizerName string) error {
	logger := log.FromContext(ctx)

	pvc := &corev1.PersistentVolumeClaim{}
	err := client.Get(ctx, namespacedName, pvc)
	if err != nil && !kubeerrors.IsNotFound(err) {
		logger.Error(err, "Failed to get PVC")
		return err
	}

	if !kubeerrors.IsNotFound(err) {
		needUpdate := common.RemoveFinalizer(ctx, client, pvc, finalizerName)
		if needUpdate {
			err = client.Update(ctx, pvc)
			if err != nil {
				logger.Error(err, "Failed to remove finalizer from PVC")
				return err
			}
		}
	}

	return nil
}
