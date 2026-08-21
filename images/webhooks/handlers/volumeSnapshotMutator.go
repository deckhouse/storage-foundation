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

package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhmutating "github.com/slok/kubewebhook/v2/pkg/webhook/mutating"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	d8commonapi "github.com/deckhouse/sds-common-lib/api/v1alpha1"
	"github.com/deckhouse/sds-common-lib/kubeclient"
	"github.com/deckhouse/sds-common-lib/slogh"
)

const (
	storageClassVolumeSnapshotAnnotationName = "storage.deckhouse.io/volumesnapshotclass"
	storageClassManagedbyLabelName           = "storage-foundation.deckhouse.io/managed-by"

	// provenanceLabelName records WHO created a VolumeSnapshot, and provenanceUserInCluster is the value for
	// one an ordinary client of the cluster API made. Only somebody present at the creation can state that,
	// which is why it is stated here and never derived later from an object that already exists: a
	// reconciler looking at a snapshot has no way to tell one that was always in the cluster from one that
	// appeared a second ago, and the consumers of this label separate exactly those two.
	//
	// The key belongs to the state-snapshotter label vocabulary and its owner is that repository. It is
	// transcribed here rather than imported because this webhook module does not depend on that API; the
	// literals are pinned by TestVolumeSnapshotProvenanceWireLiterals so a rename cannot pass unnoticed on
	// this side, and the two sides agree by that name being part of a published contract, not by a comment.
	provenanceLabelName     = "state-snapshotter.deckhouse.io/provenance"
	provenanceUserInCluster = "user-in-cluster"
)

// stateOriginOnCreate stamps the origin of a VolumeSnapshot that is being created right now, and reports
// whether it wrote anything. Two shapes are left exactly as they arrived, and each for its own reason:
//
//   - an object that ALREADY carries the key: its creator has stated its origin in the very payload being
//     admitted — the unified snapshot write-path does this for a tree node — and overwriting that would
//     erase the only truthful statement about who built the object, turning a node of a snapshot tree into
//     something this stack no longer recognises as its own;
//   - an object that arrives with a CONTROLLER owner reference: it is already part of another controller's
//     lifecycle, and this module puts no marker of any kind on somebody else's object. Nothing is lost by
//     staying silent — such a snapshot is refused domain capture on the owner reference alone, long before
//     any origin is consulted — and the alternative is a label key of this stack appearing on objects a
//     neighbouring module owns.
//
// Everything else is a snapshot a client of the cluster API is creating unclaimed, and its origin is
// recorded so that later readers can tell it from a snapshot the cluster already contained.
func stateOriginOnCreate(snapshot *snapshotv1.VolumeSnapshot) bool {
	if _, stated := snapshot.Labels[provenanceLabelName]; stated {
		return false
	}
	for _, ref := range snapshot.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return false
		}
	}
	if snapshot.Labels == nil {
		snapshot.Labels = map[string]string{}
	}
	snapshot.Labels[provenanceLabelName] = provenanceUserInCluster
	return true
}

func VolumeSnapshotMutate(ctx context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhmutating.MutatorResult, error) {
	log := slog.New(&slogh.Handler{})

	log.Debug("VolumeSnapshotMutate called")
	snapshot, ok := obj.(*snapshotv1.VolumeSnapshot)
	if !ok {
		// The only branch that returns no object, and the only one where that is right: this handler has
		// nothing to say about a kind it cannot read, and admitting it unchanged is the outcome that changes
		// nothing. Every other successful branch returns the snapshot, because by then it carries its origin.
		return &kwhmutating.MutatorResult{}, nil
	}

	log.Info("VolumeSnapshotMutate: object is VolumeSnapshot", "name", snapshot.Name, "namespace", snapshot.Namespace)

	// The origin is recorded first, before anything that can fail or return early: this admission is the one
	// moment at which the fact is knowable, and every path below that lets the object be created must carry
	// it. The webhook is registered on CREATE only, so there is no UPDATE through which an already-stated
	// origin could be revised here.
	if stateOriginOnCreate(snapshot) {
		log.Info("VolumeSnapshotMutate: recorded the in-cluster origin of a VolumeSnapshot created by a client",
			"snapshot", snapshot.Name, "namespace", snapshot.Namespace)
	}

	client, err := kubeclient.New(d8commonapi.AddToScheme,
		corev1.AddToScheme,
		storagev1.AddToScheme,
		snapshotv1.AddToScheme,
		clientgoscheme.AddToScheme,
		extv1.AddToScheme,
	)
	if err != nil {
		log.Error("VolumeSnapshotMutate: failed to create kube client", "error", err)
		return &kwhmutating.MutatorResult{}, err
	}

	if snapshot.Spec.Source.PersistentVolumeClaimName == nil {
		log.Warn("VolumeSnapshotMutate: VolumeSnapshot PersistentVolumeClaimName is nil. Skipping class defaulting", "snapshot", snapshot.Name)
		return &kwhmutating.MutatorResult{MutatedObject: snapshot}, nil
	}

	namespace := snapshot.Namespace
	pvcName := snapshot.Spec.Source.PersistentVolumeClaimName

	pvc := &corev1.PersistentVolumeClaim{}
	err = client.Get(ctx, types.NamespacedName{Name: *pvcName, Namespace: namespace}, pvc)
	if err != nil {
		log.Error("VolumeSnapshotMutate: failed to get PVC", "name", *pvcName, "namespace", namespace, "error", err)
		return &kwhmutating.MutatorResult{}, err
	}

	log.Info("VolumeSnapshotMutate: found PVC", "name", pvc.Name, "namespace", pvc.Namespace, "storageClassName", pvc.Spec.StorageClassName)

	if pvc.Spec.StorageClassName == nil {
		log.Error("VolumeSnapshotMutate: PVC StorageClassName is nil", "pvc", pvc.Name)
		return &kwhmutating.MutatorResult{}, errors.New("PVC StorageClassName is nil")
	}

	sc := &storagev1.StorageClass{}
	err = client.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName, Namespace: namespace}, sc)
	if err != nil {
		log.Error("VolumeSnapshotMutate: failed to get StorageClass", "name", *pvc.Spec.StorageClassName, "namespace", namespace, "error", err)
		return &kwhmutating.MutatorResult{}, err
	}

	log.Info("VolumeSnapshotMutate: found StorageClass", "name", sc.Name, "provisioner", sc.Provisioner)

	managedBy, managed := sc.Labels[storageClassManagedbyLabelName]
	if !managed {
		log.Info("VolumeSnapshotMutate: StorageClass", sc.Name, " not managed by Deckhouse")

		if snapshot.Spec.VolumeSnapshotClassName == nil {
			if volumeSnapshotClassName, ok := sc.Annotations[storageClassVolumeSnapshotAnnotationName]; ok {
				log.Info("VolumeSnapshotMutate: StorageClass has volume snapshot class name annotation, set it in VolumeSnapshot", "name", volumeSnapshotClassName)
				snapshot.Spec.VolumeSnapshotClassName = &volumeSnapshotClassName
			}
		}
		return &kwhmutating.MutatorResult{MutatedObject: snapshot}, nil
	}

	log.Info("VolumeSnapshotMutate: StorageClass is managed by module", "storageClass", sc.Name, "managedBy", managedBy)

	volumeSnapshotClassName, ok := sc.Annotations[storageClassVolumeSnapshotAnnotationName]
	if !ok {
		log.Error("VolumeSnapshotMutate: StorageClass does not have volume snapshot class name annotation", "name", sc.Name)
		return &kwhmutating.MutatorResult{}, errors.New("StorageClass does not have volume snapshot class name annotation")
	}

	if snapshot.Spec.VolumeSnapshotClassName != nil && *snapshot.Spec.VolumeSnapshotClassName != volumeSnapshotClassName {
		log.Error("VolumeSnapshotMutate: if VolumeSnapshotClassName is set, it must match the StorageClass annotation", "snapshotVolumeSnapshotClassName", *snapshot.Spec.VolumeSnapshotClassName, "storageClassAnnotation", volumeSnapshotClassName)
		return &kwhmutating.MutatorResult{}, fmt.Errorf("VolumeSnapshotMutate: if VolumeSnapshotClassName is set, it must match the StorageClass annotation, snapshotVolumeSnapshotClassName %s, storageClassAnnotation %s", *snapshot.Spec.VolumeSnapshotClassName, volumeSnapshotClassName)
	}

	log.Info("VolumeSnapshotMutate: StorageClass has volume snapshot class name annotation, set it in VolumeSnapshot", "name", volumeSnapshotClassName)
	snapshot.Spec.VolumeSnapshotClassName = &volumeSnapshotClassName
	return &kwhmutating.MutatorResult{
		MutatedObject: snapshot,
	}, nil
}
