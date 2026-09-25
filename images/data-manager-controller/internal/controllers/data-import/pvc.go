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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// EnsurePVC creates or updates the PVC from the template and returns the current object. pvcNamespace is
// where the PVC lives, resourceName is the owning DataImport — they differ for PopulateData's internal volume.
func EnsurePVC(
	ctx context.Context,
	client client.Client,
	pvcNamespace string,
	resourceName types.NamespacedName,
	pvcTemplate *dev1alpha1.PersistentVolumeClaimTemplateSpec,
	dataSourceRef *corev1.TypedObjectReference,
) (*corev1.PersistentVolumeClaim, error) {
	logger := log.FromContext(ctx).WithValues("pvcName", pvcTemplate.Name)
	logger.Info("Ensuring PVC")

	newPVC := makePVC(pvcTemplate, dataSourceRef, pvcNamespace, resourceName)

	namespacedName := types.NamespacedName{
		Namespace: pvcNamespace,
		Name:      pvcTemplate.Name,
	}

	oldPVC := &corev1.PersistentVolumeClaim{}
	err := client.Get(ctx, namespacedName, oldPVC)
	if err != nil && !kubeerrors.IsNotFound(err) {
		logger.Error(err, "Failed to get PVC")
		return nil, err
	}

	if kubeerrors.IsNotFound(err) {
		// PVC doesn't exist, create new PVC
		logger.Info("Creating new PVC")

		if err := client.Create(ctx, newPVC); err != nil {
			logger.Error(err, "Failed to create PVC")
			return nil, err
		}

		logger.Info("Successfully created PVC")
		return newPVC, nil
	}

	// PVC exists, check its state and handle accordingly
	return handleExistingPVC(ctx, client, oldPVC, newPVC)
}

// makePVC builds the PVC object. The storage-manager annotations always carry the owning DataImport's
// namespace/name, not pvcNamespace: server_pod.go and cmd/main.go's map-func resolve the DataImport through them.
func makePVC(
	pvcTemplate *dev1alpha1.PersistentVolumeClaimTemplateSpec,
	dataSourceRef *corev1.TypedObjectReference,
	pvcNamespace string,
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
			Namespace: pvcNamespace,
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

func handleExistingPVC(ctx context.Context, client client.Client, existingPVC *corev1.PersistentVolumeClaim, newPVC *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	logger := log.FromContext(ctx).WithValues("pvcNamespace", existingPVC.Namespace, "pvcName", existingPVC.Name)
	logger.Info("Handling existing PVC")

	// Check if PVC spec needs to be updated
	if needsPVCSpecUpdate(existingPVC, newPVC) {
		logger.Info("Updating PVC")

		// TODO: we update only spec. What if meta changed (labels, annotations etc)?
		existingPVC.Spec = newPVC.Spec

		if err := client.Update(ctx, existingPVC); err != nil {
			logger.Error(err, "Failed to update PVC")
			return nil, err
		}
		logger.Info("Successfully updated PVC")
		return existingPVC, nil
	}
	logger.Info("PVC is up to date")
	return existingPVC, nil
}

func needsPVCSpecUpdate(existingPVC *corev1.PersistentVolumeClaim, newPVC *corev1.PersistentVolumeClaim) bool {
	if !reflect.DeepEqual(existingPVC.Spec.AccessModes, newPVC.Spec.AccessModes) {
		return true
	}
	if !ptrEqual(existingPVC.Spec.StorageClassName, newPVC.Spec.StorageClassName) {
		return true
	}
	if !ptrEqual(existingPVC.Spec.VolumeMode, newPVC.Spec.VolumeMode) {
		return true
	}

	// resource.Quantity carries unexported state -- the parsed value, the suffix format and a cached
	// string -- so reflect.DeepEqual compares the representation and not the amount. The same request
	// survives a round-trip through the API server in a different representation, and comparing it that
	// way reports a difference that is not there. What follows from a false positive is not a harmless
	// extra write: nearly all of PersistentVolumeClaim.spec is immutable, so the update is rejected and
	// the reconcile fails on every pass. Cmp compares the amounts.
	existingStorage := existingPVC.Spec.Resources.Requests[corev1.ResourceStorage]
	newStorage := newPVC.Spec.Resources.Requests[corev1.ResourceStorage]
	return existingStorage.Cmp(newStorage) != 0
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

// internalPVCStatus classifies the internal scratch PVC without touching the API: the importer pod is its
// first consumer, so a WaitForFirstConsumer StorageClass needs no dummy consumer to unblock it.
func internalPVCStatus(pvc *corev1.PersistentVolumeClaim) TargetStatus {
	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		return TargetStatusReady
	case corev1.ClaimPending:
		return TargetStatusPending
	default:
		return TargetStatusFailed
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

// scratchVolumeFSType reports the filesystem the scratch volume was ACTUALLY formatted with, read off its
// bound PersistentVolume (spec.csi.fsType). It MUST be called while that volume still exists: deleting the
// scratch PVC takes the PV with it, the captured artifact records no filesystem type, and the import-side
// consumer only attaches after status.data.artifactRef appears — by then the value exists nowhere and
// cannot be reconstructed. It is read rather than derived from the StorageClass parameters on purpose: the
// class can be edited or recreated after provisioning, so it is not evidence about this volume.
//
// It returns a string and no error deliberately. The caller publishes the result next to
// status.data.artifactRef of an artifact that is already durable, so a missing filesystem type must be
// structurally unable to fail or delay that publication. Every miss yields the empty string, which the API
// contract defines as "not known": a Block volume (no filesystem at all), an unbound claim, a non-CSI
// volume, a driver that recorded no fsType, or a lost race with the volume's destruction. Misses are logged
// so an unexpectedly empty field stays diagnosable.
func scratchVolumeFSType(ctx context.Context, c client.Reader, pvc *corev1.PersistentVolumeClaim) string {
	logger := log.FromContext(ctx).WithValues("pvcNamespace", pvc.Namespace, "pvcName", pvc.Name)

	// A raw block volume carries no filesystem. Gate on the claim's volumeMode rather than on the PV field
	// happening to be empty: a driver may copy the StorageClass fsType parameter onto the PV even for a
	// Block volume, and publishing that would assert a filesystem the volume does not have.
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return ""
	}
	if pvc.Spec.VolumeName == "" {
		logger.Info("Scratch PVC has no bound volume; filesystem type is unknown")
		return ""
	}

	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		logger.Error(err, "Failed to read the scratch PersistentVolume; filesystem type is unknown",
			"pvName", pvc.Spec.VolumeName)
		return ""
	}
	if pv.Spec.CSI == nil {
		logger.Info("Scratch PersistentVolume is not a CSI volume; filesystem type is unknown",
			"pvName", pv.Name)
		return ""
	}
	return pv.Spec.CSI.FSType
}

// DeleteScratchPVC removes the finalizer from and deletes the scratch PVC. Idempotent: tolerates the
// PVC already being gone.
func DeleteScratchPVC(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) error {
	logger := log.FromContext(ctx).WithValues("pvcNamespace", pvc.Namespace, "pvcName", pvc.Name)

	base := pvc.DeepCopy()
	if controllerutil.RemoveFinalizer(pvc, dev1alpha1.StorageManagerFinalizerName) {
		// metadata.finalizers has more than one writer, so the patch carries an optimistic lock; a
		// Conflict surfaces to the caller unretried.
		if err := c.Patch(ctx, pvc, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !kubeerrors.IsNotFound(err) {
			logger.Error(err, "Failed to remove finalizer from scratch PVC")
			return err
		}
	}

	if err := c.Delete(ctx, pvc); err != nil && !kubeerrors.IsNotFound(err) {
		logger.Error(err, "Failed to delete scratch PVC")
		return err
	}
	logger.Info("Deleted scratch PVC")
	return nil
}

// GetPVC fetches a PVC by key, returning (nil, nil) when it does not exist.
func GetPVC(ctx context.Context, c client.Client, pvcKey types.NamespacedName) (*corev1.PersistentVolumeClaim, error) {
	pvc := new(corev1.PersistentVolumeClaim)
	if err := c.Get(ctx, pvcKey, pvc); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	return pvc, nil
}

// RemovePVCFinalizer removes finalizerName from di's CreatePVC target PVC, but only when the PVC's
// storage-manager annotations name di. Without that guard, deleting a DataImport whose spec.pvcTemplate
// names a PVC that another DataImport already owns would strip the owner's finalizer from underneath it,
// letting a user's `kubectl delete pvc` on the still-uploading owner's target succeed instantly and the
// owner then recreate the PVC it observes as gone. Idempotent.
func RemovePVCFinalizer(ctx context.Context, c client.Client, di *dev1alpha1.DataImport, finalizerName string) error {
	pvc, err := getTargetPVC(ctx, c, di)
	if err != nil {
		return err
	}
	if pvc == nil {
		return nil
	}
	if !isTargetPVCOwnedBy(pvc, di) {
		return nil
	}

	base := pvc.DeepCopy()
	if !controllerutil.RemoveFinalizer(pvc, finalizerName) {
		return nil
	}
	return patchPVCRemoveFinalizer(ctx, c, pvc, base)
}

// getTargetPVC returns the CreatePVC target PVC (spec.pvcTemplate.metadata.name in the DataImport's
// namespace), or (nil, nil) when it does not exist.
func getTargetPVC(ctx context.Context, c client.Client, di *dev1alpha1.DataImport) (*corev1.PersistentVolumeClaim, error) {
	if di.Spec.PvcTemplate == nil || di.Spec.PvcTemplate.Name == "" {
		return nil, nil
	}
	key := types.NamespacedName{Namespace: di.Namespace, Name: di.Spec.PvcTemplate.Name}
	return GetPVC(ctx, c, key)
}

// isTargetPVCOwnedBy reports whether the PVC's storage-manager annotations name this DataImport.
func isTargetPVCOwnedBy(pvc *corev1.PersistentVolumeClaim, di *dev1alpha1.DataImport) bool {
	return pvc.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] == di.Namespace &&
		pvc.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] == di.Name
}

// patchPVCRemoveFinalizer removes our finalizer from pvc in one Patch. metadata.finalizers is a list
// field with more than one writer, so the patch carries an optimistic lock; a Conflict is returned to
// the caller, never retried here. NotFound is success.
func patchPVCRemoveFinalizer(ctx context.Context, c client.Client, pvc, base *corev1.PersistentVolumeClaim) error {
	if err := c.Patch(ctx, pvc, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !kubeerrors.IsNotFound(err) {
		return err
	}
	return nil
}

// reconcileTargetPVCRelease lets a user-initiated delete of the failed target PVC complete: when the PVC
// is ours, being deleted and still carries our finalizer, it is removed; every other state issues no write.
func (r *DataImportReconciler) reconcileTargetPVCRelease(ctx context.Context) error {
	pvc, err := getTargetPVC(ctx, r.Client, r.dataImport)
	if err != nil {
		return err
	}
	if pvc == nil {
		return nil
	}
	if pvc.DeletionTimestamp == nil {
		return nil
	}
	if !isTargetPVCOwnedBy(pvc, r.dataImport) {
		return nil
	}
	if !controllerutil.ContainsFinalizer(pvc, dev1alpha1.StorageManagerFinalizerName) {
		return nil
	}

	base := pvc.DeepCopy()
	controllerutil.RemoveFinalizer(pvc, dev1alpha1.StorageManagerFinalizerName)
	return patchPVCRemoveFinalizer(ctx, r.Client, pvc, base)
}
