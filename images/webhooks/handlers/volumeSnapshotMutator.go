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
	"encoding/json"
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	d8commonapi "github.com/deckhouse/sds-common-lib/api/v1alpha1"
	"github.com/deckhouse/sds-common-lib/kubeclient"
	"github.com/deckhouse/sds-common-lib/logger"
	snapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
)

const (
	noProvisioner = "kubernetes.io/no-provisioner"

	storageClassVolumeSnapshotAnnotationName = "storage.deckhouse.io/volumesnapshotclass"

	// storageClassManagedByLabelName is the label a storage module puts on every StorageClass it makes from
	// its own resources; the value names the module or its controller and differs between modules.
	storageClassManagedByLabelName = "storage.deckhouse.io/managed-by"

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
func stateOriginOnCreate(snapshot *snapv1.VolumeSnapshot) bool {
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

// defaultModeOnCreate gives a VolumeSnapshot created without spec.mode the Capture mode. The schema
// declares no default for the field (see restoreModeOnUpdate for why), so the default lives here.
func defaultModeOnCreate(snapshot *snapv1.VolumeSnapshot) {
	if snapshot.Spec.Mode == "" {
		snapshot.Spec.Mode = snapv1.VolumeSnapshotModeCapture
	}
}

// volumeSnapshotModeOnly decodes only spec.mode of the stored object, skipping metadata and status.
type volumeSnapshotModeOnly struct {
	Spec struct {
		Mode string `json:"mode"`
	} `json:"spec"`
}

// restoreModeOnUpdate restores spec.mode dropped by an upstream-typed client's full update (e.g. the
// external-provisioner's finalizer write).
func restoreModeOnUpdate(oldRaw []byte, snapshot *snapv1.VolumeSnapshot) error {
	// An explicit mode is not compared with the stored one on purpose: a change is rejected by the CRD rule.
	if snapshot.Spec.Mode != "" {
		return nil
	}

	oldVS := new(volumeSnapshotModeOnly)
	if err := json.Unmarshal(oldRaw, oldVS); err != nil {
		return fmt.Errorf("decode the stored VolumeSnapshot: %w", err)
	}

	if oldVS.Spec.Mode == "" {
		snapshot.Spec.Mode = snapv1.VolumeSnapshotModeCapture
		return nil
	}

	snapshot.Spec.Mode = oldVS.Spec.Mode
	return nil
}

func VolumeSnapshotMutate(ctx context.Context, ar *model.AdmissionReview, obj metav1.Object) (*kwhmutating.MutatorResult, error) {
	log := Log().Named("volume-snapshot-mutator")

	log.Debug("VolumeSnapshotMutate called")
	snapshot, ok := obj.(*snapv1.VolumeSnapshot)
	if !ok {
		// The only branch that returns no object, and the only one where that is right: this handler has
		// nothing to say about a kind it cannot read, and admitting it unchanged is the outcome that changes
		// nothing. Every other successful branch returns the snapshot, because by then it carries what this
		// handler added to it: its mode, and for a PVC-source create its origin.
		return &kwhmutating.MutatorResult{}, nil
	}

	log.Info("VolumeSnapshotMutate: object is VolumeSnapshot", "name", snapshot.Name, "namespace", snapshot.Namespace)

	// Update only restores spec.mode: the create-time logic below must not run, as the source PVC may already be gone.
	if ar.Operation == model.OperationUpdate {
		err := restoreModeOnUpdate(ar.OldObjectRaw, snapshot)
		if err != nil {
			return &kwhmutating.MutatorResult{}, err
		}
		return &kwhmutating.MutatorResult{MutatedObject: snapshot}, nil
	}

	// The origin is recorded first, before anything that can fail or return early: this admission is the one
	// moment at which the fact is knowable, and every path below that lets the object be created must carry
	// it. Updates returned above. Only a PVC-source snapshot gets it; others come here just for the default mode.
	if snapshot.Spec.Source.PersistentVolumeClaimName != nil && stateOriginOnCreate(snapshot) {
		log.Info("VolumeSnapshotMutate: recorded the in-cluster origin of a VolumeSnapshot created by a client",
			"snapshot", snapshot.Name, "namespace", snapshot.Namespace)
	}

	defaultModeOnCreate(snapshot)

	if snapshot.Spec.Source.PersistentVolumeClaimName == nil {
		log.Warn("VolumeSnapshotMutate: VolumeSnapshot PersistentVolumeClaimName is nil. Skipping class defaulting", "snapshot", snapshot.Name)
		return &kwhmutating.MutatorResult{MutatedObject: snapshot}, nil
	}

	cl, err := kubeclient.New(d8commonapi.AddToScheme,
		corev1.AddToScheme,
		storagev1.AddToScheme,
		snapshotv1.AddToScheme,
		clientgoscheme.AddToScheme,
		extv1.AddToScheme,
	)
	if err != nil {
		log.Error("VolumeSnapshotMutate: failed to create kube client", logger.Err(err))
		return &kwhmutating.MutatorResult{}, err
	}

	sc, err := sourceStorageClass(ctx, cl, snapshot)
	if err != nil {
		return &kwhmutating.MutatorResult{}, err
	}

	if err := defaultSnapshotClass(snapshot, sc); err != nil {
		return &kwhmutating.MutatorResult{}, err
	}

	if err := requireSnapshotClass(ctx, cl, snapshot, sc); err != nil {
		return &kwhmutating.MutatorResult{}, err
	}
	return &kwhmutating.MutatorResult{MutatedObject: snapshot}, nil
}

// sourceStorageClass reads the StorageClass of the claim a PVC-source VolumeSnapshot is taken of.
func sourceStorageClass(ctx context.Context, cl client.Reader, snapshot *snapv1.VolumeSnapshot) (*storagev1.StorageClass, error) {
	log := Log().Named("volume-snapshot-mutator")

	namespace := snapshot.Namespace
	pvcName := *snapshot.Spec.Source.PersistentVolumeClaimName

	pvc := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: namespace}, pvc); err != nil {
		log.Error("failed to get the PVC",
			slog.String("name", pvcName), slog.String("namespace", namespace), logger.Err(err))
		return nil, err
	}

	log.Info("VolumeSnapshotMutate: found PVC", "name", pvc.Name, "namespace", pvc.Namespace, "storageClassName", pvc.Spec.StorageClassName)

	if pvc.Spec.StorageClassName == nil {
		log.Error("VolumeSnapshotMutate: PVC StorageClassName is nil", "pvc", pvc.Name)
		return nil, errors.New("PVC StorageClassName is nil")
	}

	sc := &storagev1.StorageClass{}
	if err := cl.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		log.Error("failed to get the StorageClass",
			slog.String("name", *pvc.Spec.StorageClassName), logger.Err(err))
		return nil, err
	}

	log.Info("VolumeSnapshotMutate: found StorageClass", "name", sc.Name, "provisioner", sc.Provisioner)
	return sc, nil
}

// storageClassManagedBy returns the module that manages the StorageClass, or "" for a class no module does.
func storageClassManagedBy(sc *storagev1.StorageClass) string {
	return sc.Labels[storageClassManagedByLabelName]
}

// defaultSnapshotClass fills an empty spec.volumeSnapshotClassName from the StorageClass annotation and, on
// a class a storage module manages, refuses a request naming another VolumeSnapshotClass. The module made
// the StorageClass and its snapshot class as a pair, so a snapshot taken with a different one is not what
// the user asked for; rewriting the request would hide that, which is why it is refused instead. On a class
// no module manages the annotation is only a default, and a class the user named is left alone.
//
// A class without the annotation leaves the request as it came, managed or not. Not every module class
// carries one — a driver that takes no snapshots has no class to name — and refusing here would also
// refuse a request that names its class explicitly, the one way left to snapshot such a volume. The
// snapshot controller then reports what is wrong with the request on the snapshot itself.
func defaultSnapshotClass(snapshot *snapv1.VolumeSnapshot, sc *storagev1.StorageClass) error {
	log := Log().Named("volume-snapshot-mutator")

	managedBy := storageClassManagedBy(sc)
	annotated, ok := sc.Annotations[storageClassVolumeSnapshotAnnotationName]
	if !ok {
		log.Info("VolumeSnapshotMutate: StorageClass has no volume snapshot class name annotation, leaving the class as requested",
			"storageClass", sc.Name, "managedBy", managedBy)
		return nil
	}

	requested := snapshot.Spec.VolumeSnapshotClassName
	if requested == nil {
		log.Info("VolumeSnapshotMutate: StorageClass has volume snapshot class name annotation, set it in VolumeSnapshot",
			"storageClass", sc.Name, "name", annotated)
		snapshot.Spec.VolumeSnapshotClassName = &annotated
		return nil
	}

	if managedBy == "" || *requested == annotated {
		return nil
	}

	log.Error("VolumeSnapshotMutate: if VolumeSnapshotClassName is set, it must match the StorageClass annotation",
		"storageClass", sc.Name, "managedBy", managedBy,
		"snapshotVolumeSnapshotClassName", *requested, "storageClassAnnotation", annotated)
	return fmt.Errorf("VolumeSnapshotMutate: StorageClass %s is managed by %s and takes snapshots with VolumeSnapshotClass %s, "+
		"so the VolumeSnapshot must not request %s", sc.Name, managedBy, annotated, *requested)
}

func sourceDriver(ctx context.Context, cl client.Reader, snapshot *snapv1.VolumeSnapshot, sc *storagev1.StorageClass) (string, bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: *snapshot.Spec.Source.PersistentVolumeClaimName, Namespace: snapshot.Namespace}
	if err := cl.Get(ctx, key, pvc); err != nil {
		return "", false, err
	}

	if pvc.Spec.VolumeName == "" {
		return sc.Provisioner, sc.Provisioner != noProvisioner, nil
	}

	pv := &corev1.PersistentVolume{}
	if err := cl.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return "", false, err
	}
	if pv.Spec.CSI == nil {
		return "", false, nil
	}
	return pv.Spec.CSI.Driver, true, nil
}

func requireSnapshotClass(ctx context.Context, cl client.Reader, snapshot *snapv1.VolumeSnapshot, sc *storagev1.StorageClass) error {
	log := Log().Named("volume-snapshot-mutator")

	driver, check, err := sourceDriver(ctx, cl, snapshot, sc)
	if err != nil {
		log.Error("failed to resolve the CSI driver of the source PVC", logger.Err(err))
		return err
	}
	if !check {
		log.Info("VolumeSnapshotMutate: the source volume has no CSI source, not checking the VolumeSnapshotClass",
			"snapshot", snapshot.Name, "namespace", snapshot.Namespace)
		return nil
	}

	pvcName := *snapshot.Spec.Source.PersistentVolumeClaimName

	classes := &snapshotv1.VolumeSnapshotClassList{}
	if err := cl.List(ctx, classes); err != nil {
		log.Error("failed to list the VolumeSnapshotClasses", logger.Err(err))
		return err
	}

	if name := snapshot.Spec.VolumeSnapshotClassName; name != nil && *name != "" {
		for i := range classes.Items {
			class := &classes.Items[i]
			if class.Name != *name {
				continue
			}
			if class.Driver != driver {
				return fmt.Errorf("VolumeSnapshotMutate: VolumeSnapshotClass %s belongs to the CSI driver %s, but PVC %s/%s of StorageClass %s "+
					"is served by the CSI driver %s, so the VolumeSnapshot must use a VolumeSnapshotClass of that driver",
					*name, class.Driver, snapshot.Namespace, pvcName, sc.Name, driver)
			}
			return nil
		}
		return fmt.Errorf("VolumeSnapshotMutate: VolumeSnapshotClass %s does not exist, so a VolumeSnapshot of PVC %s/%s "+
			"served by the CSI driver %s cannot be taken", *name, snapshot.Namespace, pvcName, driver)
	}

	for i := range classes.Items {
		if classes.Items[i].Driver == driver {
			return nil
		}
	}
	return fmt.Errorf("VolumeSnapshotMutate: there is no VolumeSnapshotClass for the CSI driver %s of PVC %s/%s (StorageClass %s), "+
		"so snapshots of this volume are not available: the driver does not support snapshots or its module is not enabled",
		driver, snapshot.Namespace, pvcName, sc.Name)
}
