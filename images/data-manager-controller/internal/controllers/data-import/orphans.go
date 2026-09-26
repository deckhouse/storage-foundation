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

package dataimport

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// releaseOrphanedResources cleans up after a DataImport that is gone without its deletion branch having run.
//
// That happens when the object is deleted inside its first reconcile. The reconcile creates the importer
// Deployment and the PVCs — each carrying this module's finalizer — straight away, but the DataImport's own
// finalizer is only written by the deferred status patch at the end, and the API server does not let a
// finalizer be added to an object that is already being deleted. The DataImport then disappears at once,
// cleanupDataImport never runs, and its PVCs keep a finalizer nobody will remove: they stay Terminating
// forever and hold up the deletion of their namespace, or of the whole cluster.
//
// The resources are found without the DataImport, by the app label and the storage-manager annotations the
// module stamps on everything it creates for an import. Every such object reaches this function: the
// controller watches labelled Deployments and PVCs and maps them back to their DataImport, whose Get then
// reports NotFound.
//
// The importer Deployment is deleted. Every PVC loses this module's finalizer; the internal scratch PVC in
// the controller namespace is deleted as well, while a target PVC in the user's namespace never is — it is
// the user's, exactly as in cleanupDataImport.
func (r *DataImportReconciler) releaseOrphanedResources(ctx context.Context, key types.NamespacedName) error {
	logger := log.FromContext(ctx)
	byLabel := client.MatchingLabels{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataImportValue}

	deployments := &appsv1.DeploymentList{}
	if err := r.Client.List(ctx, deployments, client.InNamespace(r.Config.ControllerNamespace), byLabel); err != nil {
		return fmt.Errorf("list importer Deployments: %w", err)
	}
	for i := range deployments.Items {
		deploy := &deployments.Items[i]
		if !annotatedFor(deploy.Annotations, key) {
			continue
		}
		logger.Info("Deleting the importer Deployment of a DataImport that is gone", "deployment", deploy.Name)
		if err := r.Client.Delete(ctx, deploy); err != nil && !kubeerrors.IsNotFound(err) {
			return fmt.Errorf("delete orphaned importer Deployment %s: %w", deploy.Name, err)
		}
	}

	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.Client.List(ctx, pvcs, byLabel); err != nil {
		return fmt.Errorf("list import PVCs: %w", err)
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if !annotatedFor(pvc.Annotations, key) {
			continue
		}
		scratch := pvc.Namespace == r.Config.ControllerNamespace
		if controllerutil.ContainsFinalizer(pvc, dev1alpha1.StorageManagerFinalizerName) {
			logger.Info("Releasing a PVC of a DataImport that is gone", "pvc", client.ObjectKeyFromObject(pvc), "scratch", scratch)
			base := pvc.DeepCopy()
			controllerutil.RemoveFinalizer(pvc, dev1alpha1.StorageManagerFinalizerName)
			if err := patchPVCRemoveFinalizer(ctx, r.Client, pvc, base); err != nil {
				return fmt.Errorf("remove finalizer from orphaned PVC %s: %w", client.ObjectKeyFromObject(pvc), err)
			}
		}
		if scratch && pvc.DeletionTimestamp == nil {
			if err := r.Client.Delete(ctx, pvc); err != nil && !kubeerrors.IsNotFound(err) {
				return fmt.Errorf("delete orphaned scratch PVC %s: %w", pvc.Name, err)
			}
		}
	}
	return nil
}

// annotatedFor reports whether an object's storage-manager annotations name this DataImport.
func annotatedFor(annotations map[string]string, key types.NamespacedName) bool {
	return annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] == key.Namespace &&
		annotations[dev1alpha1.AnnotationStorageManagerNameKey] == key.Name
}
