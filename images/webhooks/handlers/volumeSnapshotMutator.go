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

	// TODO: migrate to storage-foundation.deckhouse.io/managed-by after every producer of a managed
	// StorageClass is switched to it, in one synchronous change. Until then this platform-wide label is
	// the only ownership marker StorageClasses actually carry, and the narrower module-specific name
	// matched nothing, which left the managed branch below dead: a managed class without the snapshot
	// class annotation silently produced a VolumeSnapshot with no class at all.
	storageClassManagedbyLabelName = "storage.deckhouse.io/managed-by"
)

// resolveVolumeSnapshotClass decides which VolumeSnapshotClass a VolumeSnapshot must use, given the
// StorageClass behind its PVC and the class the user asked for (nil when unset). It reports the class to
// write and whether the snapshot has to be mutated at all.
//
// Ownership is decided by the presence of the managed-by label alone, with no allowlist of values: any
// StorageClass carrying it is ours, and a missing snapshot class annotation on it is a configuration
// error we refuse at admission rather than paper over. Silently admitting such a snapshot is what makes
// it hang half-accepted later, so the refusal names the StorageClass and the annotation to add.
// Unlabelled classes belong to somebody else and keep the older, permissive behavior.
//
// A returned error is ALWAYS a configuration verdict about a well-formed request, never a server-side
// failure. That distinction decides which webhook may deliver it: kubewebhook maps a mutator error to
// HTTP 500, which the API server reports as "Internal error occurred", so a mutating webhook can only
// disguise a misconfiguration as a broken server. The refusal is therefore delivered by the validating
// webhook (VolumeSnapshotValidateFunc), which produces a real denial with this message; the mutator
// treats the same verdict as "nothing to default" and defers to it.
func resolveVolumeSnapshotClass(sc *storagev1.StorageClass, requested *string) (className string, mutate bool, err error) {
	annotated, hasAnnotation := sc.Annotations[storageClassVolumeSnapshotAnnotationName]

	if _, managed := sc.Labels[storageClassManagedbyLabelName]; !managed {
		if requested == nil && hasAnnotation {
			return annotated, true, nil
		}
		return "", false, nil
	}

	if !hasAnnotation {
		return "", false, fmt.Errorf("StorageClass %q is managed by %s but has no %s annotation: add the annotation with the name of the VolumeSnapshotClass to use, or set spec.volumeSnapshotClassName explicitly on a StorageClass that is not managed",
			sc.Name, storageClassManagedbyLabelName, storageClassVolumeSnapshotAnnotationName)
	}

	if requested != nil && *requested != annotated {
		return "", false, fmt.Errorf("spec.volumeSnapshotClassName %q does not match the %s annotation %q on StorageClass %q: use %q or omit the field",
			*requested, storageClassVolumeSnapshotAnnotationName, annotated, sc.Name, annotated)
	}

	return annotated, true, nil
}

// storageClassOfPVCSource resolves the StorageClass behind the VolumeSnapshot's source PVC. It reports
// found=false when there is nothing to decide on (not a PVC-source snapshot).
//
// Errors here are genuine server-side failures — no kube client, unreadable PVC or StorageClass — as
// opposed to the configuration verdicts of resolveVolumeSnapshotClass. Both webhooks let them surface as
// errors so that, with failurePolicy: Fail, an unknown cluster state fails closed instead of admitting a
// snapshot nobody can execute.
func storageClassOfPVCSource(ctx context.Context, log *slog.Logger, snapshot *snapshotv1.VolumeSnapshot) (sc *storagev1.StorageClass, found bool, err error) {
	if snapshot.Spec.Source.PersistentVolumeClaimName == nil {
		log.Warn("VolumeSnapshot has no source PVC, nothing to resolve", "snapshot", snapshot.Name)
		return nil, false, nil
	}

	client, err := kubeclient.New(d8commonapi.AddToScheme,
		corev1.AddToScheme,
		storagev1.AddToScheme,
		snapshotv1.AddToScheme,
		clientgoscheme.AddToScheme,
		extv1.AddToScheme,
	)
	if err != nil {
		log.Error("failed to create kube client", "error", err)
		return nil, false, err
	}

	namespace := snapshot.Namespace
	pvcName := *snapshot.Spec.Source.PersistentVolumeClaimName

	pvc := &corev1.PersistentVolumeClaim{}
	if err := client.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: namespace}, pvc); err != nil {
		log.Error("failed to get PVC", "name", pvcName, "namespace", namespace, "error", err)
		return nil, false, err
	}

	if pvc.Spec.StorageClassName == nil {
		log.Error("PVC StorageClassName is nil", "pvc", pvc.Name)
		return nil, false, errors.New("PVC StorageClassName is nil")
	}

	sc = &storagev1.StorageClass{}
	if err := client.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		log.Error("failed to get StorageClass", "name", *pvc.Spec.StorageClassName, "error", err)
		return nil, false, err
	}

	log.Info("resolved StorageClass of source PVC", "pvc", pvc.Name, "storageClass", sc.Name, "provisioner", sc.Provisioner)
	return sc, true, nil
}

func VolumeSnapshotMutate(ctx context.Context, _ *model.AdmissionReview, obj metav1.Object) (*kwhmutating.MutatorResult, error) {
	log := slog.New(slogh.NewHandler(slogh.Config{}))

	log.Debug("VolumeSnapshotMutate called")
	snapshot, ok := obj.(*snapshotv1.VolumeSnapshot)
	if !ok {
		return &kwhmutating.MutatorResult{}, nil
	}

	log.Info("VolumeSnapshotMutate: object is VolumeSnapshot", "name", snapshot.Name, "namespace", snapshot.Namespace)

	sc, found, err := storageClassOfPVCSource(ctx, log, snapshot)
	if err != nil {
		return &kwhmutating.MutatorResult{}, err
	}
	if !found {
		return &kwhmutating.MutatorResult{}, nil
	}

	return mutationForStorageClass(log, sc, snapshot), nil
}

// mutationForStorageClass turns the class decision into a patch. A configuration verdict is deliberately
// NOT propagated as an error: the request is refused by the validating webhook, which runs after mutation
// and can state the reason plainly, whereas erroring here would surface as "Internal error occurred".
func mutationForStorageClass(log *slog.Logger, sc *storagev1.StorageClass, snapshot *snapshotv1.VolumeSnapshot) *kwhmutating.MutatorResult {
	volumeSnapshotClassName, mutate, err := resolveVolumeSnapshotClass(sc, snapshot.Spec.VolumeSnapshotClassName)
	switch {
	case err != nil:
		log.Warn("VolumeSnapshotMutate: nothing to default, the validating webhook refuses this VolumeSnapshot",
			"snapshot", snapshot.Name, "storageClass", sc.Name, "reason", err)
		return &kwhmutating.MutatorResult{}
	case !mutate:
		log.Info("VolumeSnapshotMutate: leaving VolumeSnapshot as is", "snapshot", snapshot.Name, "storageClass", sc.Name)
		return &kwhmutating.MutatorResult{}
	}

	log.Info("VolumeSnapshotMutate: setting volume snapshot class from StorageClass", "storageClass", sc.Name, "volumeSnapshotClassName", volumeSnapshotClassName)
	snapshot.Spec.VolumeSnapshotClassName = &volumeSnapshotClassName
	return &kwhmutating.MutatorResult{MutatedObject: snapshot}
}
