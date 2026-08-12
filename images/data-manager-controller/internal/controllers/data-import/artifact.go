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

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// Ownership note: the produced VolumeSnapshotContent/PersistentVolume is NOT ownerless and DataImport is
// NOT its controller owner. storage-foundation's VolumeCaptureRequest controller creates the artifact
// controller-owned by its own retainer ObjectKeeper (retainer-vcr-<vcrUID> / ret-pv-<vcrUID>) which
// FollowObject-follows the VCR — but that keeper only anchors the artifact to the VCR's lifetime, and the
// VCR garbage collector deletes a completed VCR after GC_VCR_TTL (default 24h), cascading GC to the
// artifact. So DataImport ALSO adds its own keeper (FollowObject -> DataImport) as a non-controller
// additional owner
// (object_keeper.go) to anchor the artifact to the import's lifetime until C5 adopts it under a durable
// SnapshotContent owner. Independently, DataImport pins the reclaim policy to Retain so a deleted per-run
// handle never reclaims the physical data before C5 adopts it.

// pinArtifactRetain pins the produced artifact's reclaim policy to Retain so it survives deletion of any
// per-run handle: VolumeSnapshotContent.spec.deletionPolicy=Retain or
// PersistentVolume.spec.persistentVolumeReclaimPolicy=Retain. The VolumeCaptureRequest sets the VSC's
// deletionPolicy from its VolumeSnapshotClass (often Delete), so this pin is load-bearing. Idempotent.
func (r *DataImportReconciler) pinArtifactRetain(ctx context.Context, artifact *dev1alpha1.DataArtifactReference) error {
	switch artifact.Kind {
	case artifactKindVolumeSnapshotContent:
		vsc := &snapv1.VolumeSnapshotContent{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: artifact.Name}, vsc); err != nil {
			return fmt.Errorf("get VolumeSnapshotContent %s: %w", artifact.Name, err)
		}
		if vsc.Spec.DeletionPolicy == snapv1.VolumeSnapshotContentRetain {
			return nil
		}
		updated := vsc.DeepCopy()
		updated.Spec.DeletionPolicy = snapv1.VolumeSnapshotContentRetain
		if err := r.Client.Patch(ctx, updated, client.MergeFrom(vsc)); err != nil {
			return fmt.Errorf("pin VolumeSnapshotContent %s deletionPolicy=Retain: %w", artifact.Name, err)
		}
		return nil
	case artifactKindPersistentVolume:
		pv := &corev1.PersistentVolume{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: artifact.Name}, pv); err != nil {
			return fmt.Errorf("get PersistentVolume %s: %w", artifact.Name, err)
		}
		if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain {
			return nil
		}
		updated := pv.DeepCopy()
		updated.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		if err := r.Client.Patch(ctx, updated, client.MergeFrom(pv)); err != nil {
			return fmt.Errorf("pin PersistentVolume %s reclaimPolicy=Retain: %w", artifact.Name, err)
		}
		return nil
	default:
		return fmt.Errorf("cannot pin Retain on unsupported artifact kind %q", artifact.Kind)
	}
}
