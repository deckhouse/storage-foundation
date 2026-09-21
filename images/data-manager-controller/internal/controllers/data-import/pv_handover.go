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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// annSelectedNode is the annotation kube-scheduler writes on an unbound WaitForFirstConsumer PVC to
// record its node choice. No dependency of this module exports it as a Go constant, so it is declared here.
const annSelectedNode = "volume.kubernetes.io/selected-node"

// computeCreatePVCScratchTemplate shapes the internal scratch PVC after the live target PVC rather than
// spec.pvcTemplate, because the apiserver defaults the target's volumeMode on admission and may
// retroactively assign its default StorageClass after creation — only the live object shows the class and
// mode the target will actually be judged against.
func computeCreatePVCScratchTemplate(name string, target *corev1.PersistentVolumeClaim) *dev1alpha1.PersistentVolumeClaimTemplateSpec {
	accessModes := make([]dev1alpha1.PersistentVolumeAccessMode, len(target.Spec.AccessModes))
	for i, mode := range target.Spec.AccessModes {
		accessModes[i] = dev1alpha1.PersistentVolumeAccessMode(mode)
	}

	requests := make(dev1alpha1.ResourceList, len(target.Spec.Resources.Requests))
	for resourceName, quantity := range target.Spec.Resources.Requests {
		requests[dev1alpha1.ResourceName(resourceName)] = quantity.DeepCopy()
	}

	var storageClassName *string
	if target.Spec.StorageClassName != nil {
		sc := *target.Spec.StorageClassName
		storageClassName = &sc
	}

	var volumeMode *dev1alpha1.PersistentVolumeMode
	if target.Spec.VolumeMode != nil {
		vm := dev1alpha1.PersistentVolumeMode(*target.Spec.VolumeMode)
		volumeMode = &vm
	}

	return &dev1alpha1.PersistentVolumeClaimTemplateSpec{
		PersistentVolumeClaimTemplateMetadata: dev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: name},
		PersistentVolumeClaimSpec: dev1alpha1.PersistentVolumeClaimSpec{
			AccessModes:      accessModes,
			Resources:        dev1alpha1.VolumeResourceRequirements{Requests: requests},
			StorageClassName: storageClassName,
			VolumeMode:       volumeMode,
		},
	}
}

// computeScratchNodeHint decides the CreatePVC scratch PVC's node hint from two inputs that must both
// agree before a hint is even needed: the target PVC's StorageClass binding mode and
// spec.waitForFirstConsumer. Gating on either alone fails differently:
//   - on waitForFirstConsumer alone, an Immediate class would wait forever for a scheduler annotation
//     that class never produces;
//   - on classIsWFC alone, a WaitForFirstConsumer class with waitForFirstConsumer=false would wait for
//     an annotation nobody writes, when the importer pod is free to be the scratch PVC's own first
//     consumer.
func computeScratchNodeHint(targetPVC *corev1.PersistentVolumeClaim, classIsWFC, waitForFirstConsumer bool) (nodeName string, wait bool) {
	if !classIsWFC || !waitForFirstConsumer {
		return "", false
	}
	nodeName = targetPVC.Annotations[annSelectedNode]
	return nodeName, nodeName == ""
}

// getHandoverPV fetches the PersistentVolume the scratch PVC is bound to, by its spec.volumeName.
// scratchPVC.Spec.VolumeName must already be set before this is called; reconcileHandover checks that first.
func getHandoverPV(ctx context.Context, c client.Client, scratchPVC *corev1.PersistentVolumeClaim) (*corev1.PersistentVolume, error) {
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: scratchPVC.Spec.VolumeName}, pv); err != nil {
		return nil, fmt.Errorf("get PersistentVolume %s: %w", scratchPVC.Spec.VolumeName, err)
	}
	return pv, nil
}

// isPVClaimedBy reports whether pv.spec.claimRef names pvc. An empty UID is a pre-bound reference to
// whichever claim has that name — the binder fills it in on bind — and counts as a match.
func isPVClaimedBy(pv *corev1.PersistentVolume, pvc *corev1.PersistentVolumeClaim) bool {
	ref := pv.Spec.ClaimRef
	if ref == nil {
		return false
	}
	if ref.Namespace != pvc.Namespace || ref.Name != pvc.Name {
		return false
	}
	return ref.UID == "" || ref.UID == pvc.UID
}

// patchPVClaimRefToTarget rebinds pv onto the target PVC in one Patch: pv.spec.claimRef becomes
// {apiVersion, kind, namespace, name, uid, resourceVersion} of the target PVC, the full set the PV
// controller itself writes on a bind. client.MergeFrom sends a key present in base but absent from the
// patched object as an explicit null, so an unset apiVersion/kind would clear what the binder already
// wrote there, and an unset uid would produce a pre-bound reference that binds to whichever claim later
// carries this name. claimRef has more than one writer, so the patch carries an optimistic lock; a
// Conflict is returned to the caller unretried, and it is idempotent — a pv already claimed by the target
// PVC produces no write.
func patchPVClaimRefToTarget(ctx context.Context, c client.Client, pv, base *corev1.PersistentVolume, targetPVC *corev1.PersistentVolumeClaim) error {
	if isPVClaimedBy(pv, targetPVC) {
		return nil
	}

	pv.Spec.ClaimRef = &corev1.ObjectReference{
		APIVersion:      corev1.SchemeGroupVersion.String(),
		Kind:            dev1alpha1.KindPVC,
		Namespace:       targetPVC.Namespace,
		Name:            targetPVC.Name,
		UID:             targetPVC.UID,
		ResourceVersion: targetPVC.ResourceVersion,
	}
	return c.Patch(ctx, pv, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// reconcileHandover moves the volume the scratch PVC staged the upload into onto the target PVC, then lets
// it go once acknowledged; di's conditions are mutated in memory with no status write of its own — the
// caller persists.
//
// scratchPVC must be non-nil — a scratch PVC that vanished mid-upload is the caller's decision, never seen
// here. The gate below is spec.VolumeName being set, not Status.Phase == Bound: once the claimRef patch
// moves the volume onto the target PVC, the PV controller marks the scratch PVC ClaimLost without clearing
// spec.VolumeName, and the second branch below must be reachable in that state for the handover to finish.
//
// A claimRef naming neither PVC is terminal with no write: Kubernetes cannot produce that value on this
// path, so it can only mean a hand edit of the volume, and treating it as "claimed by the scratch PVC"
// would patch over whoever actually holds it.
//
//   - Claimed by the scratch PVC: the handover has not happened yet. Patching pv.spec.claimRef onto a
//     target PVC already Bound to a DIFFERENT PersistentVolume is refused outright, with no patch issued
//     at all — the PV controller reclaims (and CSI-deletes) a dynamically provisioned volume whose claimRef
//     names a claim bound elsewhere, destroying the volume this import just filled. An unbound target PVC,
//     or one already bound to THIS PersistentVolume (a repeated handover), is safe to patch.
//   - Claimed by the target PVC: the handover already happened. The scratch PVC is deleted only once both
//     independent PV-controller events are observed — the scratch PVC ClaimLost AND the target PVC
//     actually Bound to the handed-over volume — because deleting on the first alone races the binder.
func reconcileHandover(ctx context.Context, c client.Client, di *dev1alpha1.DataImport, targetPVC, scratchPVC *corev1.PersistentVolumeClaim) (ctrl.Result, error) {
	if scratchPVC.Spec.VolumeName == "" {
		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "Awaiting the internal volume to bind before handing it over to the target PVC",
			ObservedGeneration: di.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	}

	pv, err := getHandoverPV(ctx, c, scratchPVC)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case isPVClaimedBy(pv, scratchPVC):
		if targetPVC.Spec.VolumeName != "" && targetPVC.Spec.VolumeName != pv.Name {
			return ctrl.Result{}, fmt.Errorf(
				"%w: %w: target PVC %s/%s is already bound to PersistentVolume %s, not to the imported volume %s; refusing to rebind",
				ErrTerminal, ErrTargetFailed, targetPVC.Namespace, targetPVC.Name, targetPVC.Spec.VolumeName, pv.Name)
		}

		base := pv.DeepCopy()
		if err := patchPVClaimRefToTarget(ctx, c, pv, base, targetPVC); err != nil {
			return ctrl.Result{}, err
		}

		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "Internal volume handed over to the target PVC; awaiting the target PVC to bind",
			ObservedGeneration: di.Generation,
		})
		return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil

	case isPVClaimedBy(pv, targetPVC):
		if scratchPVC.Status.Phase != corev1.ClaimLost || targetPVC.Status.Phase != corev1.ClaimBound || targetPVC.Spec.VolumeName != pv.Name {
			meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
				Type:               string(common.ConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             string(common.ReasonPending),
				Message:            "Handing the internal volume over to the target PVC",
				ObservedGeneration: di.Generation,
			})
			return ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
		}

		if err := DeleteScratchPVC(ctx, c, scratchPVC); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil

	default:
		claimant := "no claimant"
		if pv.Spec.ClaimRef != nil {
			claimant = fmt.Sprintf("%s/%s", pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
		}
		return ctrl.Result{}, fmt.Errorf(
			"%w: %w: PersistentVolume %s is claimed by %s, not by scratch PVC %s/%s or target PVC %s/%s; refusing to rebind",
			ErrTerminal, ErrTargetFailed, pv.Name, claimant,
			scratchPVC.Namespace, scratchPVC.Name, targetPVC.Namespace, targetPVC.Name)
	}
}
