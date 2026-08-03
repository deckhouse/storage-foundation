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

package dataexport

import (
	"context"
	"errors"
	"fmt"
	"log"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	. "github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/publish"
	virtv1alpha2 "github.com/deckhouse/virtualization/api/core/v1alpha2"
)

// recoveryBarrier is a safety precondition that has not been met yet. It is returned as data rather than
// written to the object, so the primitive stays testable without status mutations and a single caller
// decides how a blockage is surfaced.
//
// A barrier is not an error: nothing failed, the world simply is not ready for the next irreversible
// step. There is no timeout after which a barrier is ignored — skipping one corrupts data, while waiting
// only leaves a visibly unfinished object.
type recoveryBarrier struct {
	// Name identifies the barrier (B1..B4) so operators can map a message to this contract.
	Name string
	// Object is the concrete evidence that blocks progress: the Pod still holding the claim, the
	// VolumeAttachment still attached, the claim still terminating, the volume not yet rebound.
	Object client.ObjectKey
	// Message explains the blockage in the terms shown on the DataExport.
	Message string
}

func (b *recoveryBarrier) String() string {
	return fmt.Sprintf("%s: %s", b.Name, b.Message)
}

// barrierNoConsumerPods (B1) requires that no Pod references the export claim any more. A terminal Pod
// does not pass: phase Succeeded or Failed does not prove the kubelet finished tearing the volume down,
// and for non-attachable volumes B2 offers no independent signal. The rule covers every Pod referencing
// the claim, not only the exporter's own, which is what makes it correct for RWX volumes too.
//
// It reads live rather than from the cache, which holds only pods labelled as this module's own. A
// barrier decides whether a volume may be taken away from whoever holds it, and a filtered read cannot
// tell "no pod has it" from "no pod I am watching has it" — the one distinction the barrier exists for.
func (r *DataexportReconciler) barrierNoConsumerPods(ctx context.Context, exportPVCName string) (*recoveryBarrier, error) {
	pods := &corev1.PodList{}
	if err := r.Reader.List(ctx, pods, client.InNamespace(r.Config.ControllerNamespace)); err != nil {
		return nil, fmt.Errorf("failed to list pods in %s: %w", r.Config.ControllerNamespace, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != exportPVCName {
				continue
			}
			return &recoveryBarrier{
				Name:   "B1",
				Object: client.ObjectKeyFromObject(pod),
				Message: fmt.Sprintf("pod %s/%s still references export claim %s (phase %s); the volume is not provably unmounted until the pod is gone",
					pod.Namespace, pod.Name, exportPVCName, pod.Status.Phase),
			}, nil
		}
	}
	return nil, nil
}

// barrierNoVolumeAttachment (B2) requires that no VolumeAttachment for this volume is observable, on any
// node. The absence of one is not read as evidence about the driver's capabilities — it may have been
// deleted, may not exist yet, or may not be cached. The barrier only asserts that no attachment is active
// right now; safety for non-attachable volumes comes from B1 insisting on the pod being gone.
func (r *DataexportReconciler) barrierNoVolumeAttachment(ctx context.Context, pvName string) (*recoveryBarrier, error) {
	attachments := &storagev1.VolumeAttachmentList{}
	if err := r.Client.List(ctx, attachments); err != nil {
		return nil, fmt.Errorf("failed to list volume attachments: %w", err)
	}
	for i := range attachments.Items {
		attachment := &attachments.Items[i]
		source := attachment.Spec.Source.PersistentVolumeName
		if source == nil || *source != pvName {
			continue
		}
		return &recoveryBarrier{
			Name:   "B2",
			Object: client.ObjectKeyFromObject(attachment),
			Message: fmt.Sprintf("VolumeAttachment %s still references PV %s (attached=%t) on node %s",
				attachment.Name, pvName, attachment.Status.Attached, attachment.Spec.NodeName),
		}, nil
	}
	return nil, nil
}

// volumeHolder is who owns the volume according to the binding itself — PV.spec.claimRef — as opposed to
// who owned it when the takeover was recorded. Recovery is only ever allowed to take the volume away from
// a holder it created, so this question has to be asked of the live object, not of the record.
type volumeHolder int

const (
	// holderNobody: the volume is unbound.
	holderNobody volumeHolder = iota
	// holderSourceClaim: the volume is already back with the claim it was taken from.
	holderSourceClaim
	// holderExportClaim: the volume is held by the claim this export made for it.
	holderExportClaim
	// holderStranger: somebody else's claim owns the volume now. Recovery must not touch it.
	holderStranger
)

// classifyVolumeHolder answers who currently holds the volume. A name match is not enough for the export
// claim: an object may reuse the name after the original is gone, and the record says which UID the
// takeover actually created. Legacy takeovers carry no recorded UID, so for them the claim named in the
// binding, in our namespace, is the holder we made.
func classifyVolumeHolder(
	claimRef *corev1.ObjectReference,
	exportClaim types.NamespacedName,
	sourceClaim types.NamespacedName,
	recordedExportUID string,
) volumeHolder {
	switch {
	case claimRef == nil:
		return holderNobody
	case claimRef.Namespace == sourceClaim.Namespace && claimRef.Name == sourceClaim.Name:
		return holderSourceClaim
	case claimRef.Namespace == exportClaim.Namespace && claimRef.Name == exportClaim.Name &&
		(recordedExportUID == "" || string(claimRef.UID) == recordedExportUID):
		return holderExportClaim
	default:
		return holderStranger
	}
}

// barrierStrangerHoldsTheVolume (B3) refuses to proceed while somebody else's claim owns the volume. It
// is not a wait in the ordinary sense — nothing here is expected to resolve on its own — but it is the
// same contract: no mutation, the recovery stays owed, and a human sees what is in the way. Rebinding
// past this would take a bound volume away from its current owner.
func barrierStrangerHoldsTheVolume(pv *corev1.PersistentVolume) *recoveryBarrier {
	claimRef := pv.Spec.ClaimRef
	return &recoveryBarrier{
		Name:   "B3",
		Object: types.NamespacedName{Namespace: claimRef.Namespace, Name: claimRef.Name},
		Message: fmt.Sprintf("PV %s is currently bound to %s, which this export did not create; refusing to take the volume away from it",
			pv.Name, describeClaimRef(claimRef)),
	}
}

// barrierExportPVCGone (B3) requires that the claim holding the volume no longer exists. It is evaluated
// against the UID the volume is bound to, not against the name: when an object has taken the name after
// the holder was gone, that object is none of our business. A claim still carrying the holder UID does not
// pass while it is Terminating, because the PV controller keeps acting on the binding until it is gone.
func (r *DataexportReconciler) barrierExportPVCGone(ctx context.Context, exportPVCName string, holderUID types.UID) (*recoveryBarrier, error) {
	exportPVC, err := r.getExportPVC(ctx, exportPVCName)
	if err != nil || exportPVC == nil {
		return nil, err
	}
	if holderUID != "" && exportPVC.UID != holderUID {
		return nil, nil
	}
	return &recoveryBarrier{
		Name:   "B3",
		Object: client.ObjectKeyFromObject(exportPVC),
		Message: fmt.Sprintf("export claim %s/%s still exists (terminating=%t); the volume cannot be rebound while a claim holds it",
			exportPVC.Namespace, exportPVC.Name, exportPVC.DeletionTimestamp != nil),
	}, nil
}

// barrierBindingRestored (B4) requires the volume and the user's claim to be bound to each other again,
// confirmed by a fresh read of both. The UID comparison is what makes it meaningful: without it, a
// same-named but different claim would look like a completed restore.
func (r *DataexportReconciler) barrierBindingRestored(
	ctx context.Context,
	pvName string,
	sourcePVC types.NamespacedName,
	sourcePVCUID string,
) (*recoveryBarrier, error) {
	pv := &corev1.PersistentVolume{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		return nil, fmt.Errorf("failed to re-read PV %s: %w", pvName, err)
	}

	claimRef := pv.Spec.ClaimRef
	switch {
	case claimRef == nil:
		return &recoveryBarrier{Name: "B4", Object: client.ObjectKey{Name: pvName},
			Message: fmt.Sprintf("PV %s is not bound to any claim yet", pvName)}, nil
	case claimRef.Namespace != sourcePVC.Namespace || claimRef.Name != sourcePVC.Name:
		return &recoveryBarrier{Name: "B4", Object: client.ObjectKey{Name: pvName},
			Message: fmt.Sprintf("PV %s is bound to %s, not to %s", pvName, describeClaimRef(claimRef), sourcePVC)}, nil
	case sourcePVCUID != "" && string(claimRef.UID) != sourcePVCUID:
		return &recoveryBarrier{Name: "B4", Object: client.ObjectKey{Name: pvName},
			Message: fmt.Sprintf("PV %s names claim %s but with UID %s instead of the claim it was taken from (%s)",
				pvName, sourcePVC, claimRef.UID, sourcePVCUID)}, nil
	}

	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Client.Get(ctx, sourcePVC, claim); err != nil {
		if kubeerrors.IsNotFound(err) {
			return &recoveryBarrier{Name: "B4", Object: sourcePVC,
				Message: fmt.Sprintf("claim %s no longer exists to be bound to PV %s", sourcePVC, pvName)}, nil
		}
		return nil, fmt.Errorf("failed to re-read claim %s: %w", sourcePVC, err)
	}

	switch {
	case claim.UID != claimRef.UID:
		// The volume points at a claim UID that this object does not have: the claim it was bound to is
		// gone and a namesake took its place. Same name, different owner.
		return &recoveryBarrier{Name: "B4", Object: sourcePVC,
			Message: fmt.Sprintf("claim %s has UID %s while PV %s is bound to UID %s: the volume is not bound to this claim",
				sourcePVC, claim.UID, pvName, claimRef.UID)}, nil
	case claim.Spec.VolumeName != pvName:
		return &recoveryBarrier{Name: "B4", Object: sourcePVC,
			Message: fmt.Sprintf("claim %s names volume %q, not %s", sourcePVC, claim.Spec.VolumeName, pvName)}, nil
	}

	// Both objects name each other; the phases are the storage layer's confirmation that it agrees. Until
	// it does, the volume keeps its export-time protection.
	switch {
	case pv.Status.Phase != corev1.VolumeBound:
		return &recoveryBarrier{Name: "B4", Object: client.ObjectKey{Name: pvName},
			Message: fmt.Sprintf("PV %s is %s, not yet Bound to %s", pvName, pv.Status.Phase, sourcePVC)}, nil
	case claim.Status.Phase != corev1.ClaimBound:
		return &recoveryBarrier{Name: "B4", Object: sourcePVC,
			Message: fmt.Sprintf("claim %s is %s, not yet Bound to PV %s", sourcePVC, claim.Status.Phase, pvName)}, nil
	}

	return nil, nil
}

// takeoverRef is everything the teardown needs to know about what an export borrowed: which volume, whose
// claim it belongs to, and whatever identity can be proven about them. It is deliberately not the
// DataExport: the orphan sweep runs when the parent is already gone, and the same work has to be possible
// from annotations alone.
//
// Empty fields mean "cannot be proven", not "does not matter". Pre-UID takeovers legitimately have none of
// the UIDs, and the checks that consume them relax accordingly.
type takeoverRef struct {
	PVName       string
	PVUID        string
	ExportPVCUID string
	SourcePVCUID string
	// SourceClaim is the claim the volume must go back to. Empty for snapshot-backed exports, which
	// never take a user volume over.
	SourceClaim types.NamespacedName
	// DataExportUID is the export undoing the takeover, when there is one to name. The orphan sweep runs
	// after the parent is gone and leaves it empty, which is what makes that path's weaker guarantee
	// visible where it is used rather than assumed somewhere upstream.
	DataExportUID types.UID
}

func (t takeoverRef) tookOverAVolume() bool {
	return t.PVName != "" && t.SourceClaim.Name != ""
}

// resolveTakeover works out what this export borrowed. The recorded identity is preferred because it was
// written before the takeover; a pre-UID export has none, so the volume is located the way the orphan
// sweep locates it — by our marker and owner annotations — and the identity is whatever the annotations
// carry.
func (r *DataexportReconciler) resolveTakeover(
	ctx context.Context,
	dataExport *dev1alpha1.DataExport,
	names Names,
) (takeoverRef, error) {
	takeover := takeoverRef{DataExportUID: dataExport.UID}

	switch names.TargetKindShort {
	case dev1alpha1.KindPVCShort:
		takeover.SourceClaim = types.NamespacedName{Namespace: dataExport.Namespace, Name: dataExport.Spec.TargetRef.Name}
	case dev1alpha1.KindVirtualDiskShort:
		// The claim is the disk's, so it can only be named by asking the disk. A disk that is already
		// gone leaves nothing to give the volume back to.
		virtualDisk := &virtv1alpha2.VirtualDisk{}
		key := types.NamespacedName{Namespace: dataExport.Namespace, Name: dataExport.Spec.TargetRef.Name}
		if err := r.Client.Get(ctx, key, virtualDisk); err != nil {
			if !kubeerrors.IsNotFound(err) {
				return takeover, fmt.Errorf("failed to get VirtualDisk %s: %w", key, err)
			}
			log.Printf("VirtualDisk %s not found; nothing to return the volume to", key)
		} else if claimName := virtualDisk.Status.Target.PersistentVolumeClaim; claimName != "" {
			takeover.SourceClaim = types.NamespacedName{Namespace: dataExport.Namespace, Name: claimName}
		}
	}

	if recovery := dataExport.Status.Recovery; recovery != nil && recovery.PVName != "" {
		takeover.PVName = recovery.PVName
		takeover.PVUID = recovery.PVUID
		takeover.ExportPVCUID = recovery.ExportPVCUID
		takeover.SourcePVCUID = recovery.SourcePVCUID
		return takeover, nil
	}

	pv, err := r.findTakenOverPV(ctx, dataExport, names)
	if err != nil || pv == nil {
		return takeover, err
	}
	takeover.PVName = pv.Name
	// The UID is not carried over from this very read: comparing a value against its own source proves
	// nothing. What identifies this volume as ours is the owner annotation the lookup matched on.
	takeover.SourcePVCUID = pv.Annotations[dev1alpha1.AnnotationUserPVCUIDKey]
	return takeover, nil
}

// reconcileLiveExportRecovery undoes a takeover: it stops everything still using the volume, gets rid of
// the claim holding it, gives the volume back to its owner and only then tears the remaining export
// infrastructure down, releasing the user's claim last.
//
// It is level-based. No step number is stored anywhere: each stage reads the world, decides whether it
// has already happened, and either proceeds or reports the barrier that stops it. That is what makes it
// safe to call from the failure path, from deletion, from expiry, from the orphan sweep, and after an
// arbitrary restart.
//
// It never touches the DataExport's finalizer — it does not even take the object. Restoring the data
// plane and deciding the parent's lifecycle are different responsibilities, and only the caller knows
// which path it is on.
func (r *DataexportReconciler) reconcileLiveExportRecovery(
	ctx context.Context,
	names Names,
	takeover takeoverRef,
) (bool, *recoveryBarrier, error) {
	if blocked, err := r.stopExportConsumers(ctx, names, takeover); err != nil || blocked != nil {
		return false, blocked, err
	}
	if blocked, err := r.ensureExportPVCGone(ctx, names, takeover); err != nil || blocked != nil {
		return false, blocked, err
	}
	switch {
	case takeover.tookOverAVolume():
		if blocked, err := r.restoreSourcePVCBinding(ctx, names, takeover); err != nil || blocked != nil {
			return false, blocked, err
		}
	case takeover.PVName != "":
		// A volume we marked but took from nobody — a snapshot-backed export's own volume. There is no
		// binding to restore, only our marks to remove.
		if err := r.restoreExportedPVMetadata(ctx, takeover.PVName); err != nil {
			return false, nil, err
		}
	}
	if err := r.cleanupExportInfrastructure(ctx, names); err != nil {
		return false, nil, err
	}
	if takeover.SourceClaim.Name != "" {
		if err := r.releaseSourcePVC(ctx, takeover.SourceClaim); err != nil {
			return false, nil, err
		}
	}

	return true, nil, nil
}

// stopExportConsumers removes the exporter Deployment and waits until nothing holds the volume any more
// (B1, then B2). Deleting the Deployment is not enough on its own: a terminating pod may still have the
// volume mounted.
func (r *DataexportReconciler) stopExportConsumers(ctx context.Context, names Names, takeover takeoverRef) (*recoveryBarrier, error) {
	deploy := &appsv1.Deployment{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: names.DeployName}, deploy)
	switch {
	case err == nil:
		// The name is generated, but an object under it is only ours if it says so. Deleting somebody
		// else's Deployment because it collided with our naming would be worse than stopping here.
		if label := deploy.Labels[dev1alpha1.LabelApplicationKey]; label != dev1alpha1.LabelDataExportValue {
			return nil, fmt.Errorf("deployment %s/%s is not managed by data-exporter: missing or invalid app label",
				r.Config.ControllerNamespace, names.DeployName)
		}
		if deploy.DeletionTimestamp == nil {
			if err := r.Client.Delete(ctx, deploy); err != nil && !kubeerrors.IsNotFound(err) {
				return nil, fmt.Errorf("failed to delete export deployment %s: %w", names.DeployName, err)
			}
			log.Printf("Recovery: export deployment %s deleted", names.DeployName)
		}
	case !kubeerrors.IsNotFound(err):
		return nil, fmt.Errorf("failed to get export deployment %s: %w", names.DeployName, err)
	}

	if blocked, err := r.barrierNoConsumerPods(ctx, names.ExportPVCName); err != nil || blocked != nil {
		return blocked, err
	}
	if takeover.PVName == "" {
		return nil, nil
	}
	return r.barrierNoVolumeAttachment(ctx, takeover.PVName)
}

// ensureExportPVCGone gets the volume out of the claim that holds it (B3). Which claim that is comes from
// the binding as it stands right now, not from the record: a claim that reused the name after the holder
// was gone is harmless and stays untouched, while a claim that actually holds the volume and is not ours
// stops the recovery instead of losing its volume to it.
func (r *DataexportReconciler) ensureExportPVCGone(
	ctx context.Context,
	names Names,
	takeover takeoverRef,
) (*recoveryBarrier, error) {
	// An export that never borrowed a volume — snapshot-backed, or one that never got that far — has no
	// binding to reason about: the claim under our generated name in our own namespace is ours to remove.
	holderUID := types.UID("")
	if takeover.PVName != "" {
		pv := &corev1.PersistentVolume{}
		switch err := r.Client.Get(ctx, types.NamespacedName{Name: takeover.PVName}, pv); {
		case kubeerrors.IsNotFound(err):
			// Nothing holds anything; restoring the binding reports the missing volume.
		case err != nil:
			return nil, fmt.Errorf("failed to get PV %s: %w", takeover.PVName, err)
		default:
			exportClaim := types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: names.ExportPVCName}
			switch classifyVolumeHolder(pv.Spec.ClaimRef, exportClaim, takeover.SourceClaim, takeover.ExportPVCUID) {
			case holderStranger:
				return barrierStrangerHoldsTheVolume(pv), nil
			case holderExportClaim:
				holderUID = pv.Spec.ClaimRef.UID
			}
		}
	}

	exportPVC, err := r.getExportPVC(ctx, names.ExportPVCName)
	if err != nil || exportPVC == nil {
		return nil, err
	}

	// Which objects under our name we may remove: the one holding our volume, the one we recorded, or —
	// when no volume was borrowed — the one our own naming produced. Anything else came from elsewhere
	// and holds nothing of ours. A snapshot export borrows nothing by definition, so the claim it
	// provisioned is its own whether or not the sweep reached it through the volume behind that claim.
	borrowedNothing := takeover.PVName == "" || names.TargetKindShort == dev1alpha1.KindSnapshotShort
	ours := (holderUID != "" && exportPVC.UID == holderUID) ||
		(takeover.ExportPVCUID != "" && string(exportPVC.UID) == takeover.ExportPVCUID) ||
		borrowedNothing
	// A claim that names another export in its creation marker overrides all of that. The reasons above
	// infer ownership from a binding or from our own naming; this is the claim's own statement about who
	// made it, and it is the only evidence here that a stranger cannot produce by accident. The orphan
	// sweep runs without a parent to compare against and keeps the weaker guarantee.
	if claimMarkerNamesAnotherExport(takeover.DataExportUID, exportPVC) {
		ours = false
	}
	if !ours {
		if owner := exportPVC.Annotations[dev1alpha1.AnnotationDataExportUIDKey]; owner != "" {
			log.Printf("Recovery: claim %s/%s under our name was created by DataExport UID %s; leaving it untouched",
				exportPVC.Namespace, exportPVC.Name, owner)
		} else {
			log.Printf("Recovery: claim %s/%s under our name holds nothing of ours (UID %s); leaving it untouched",
				exportPVC.Namespace, exportPVC.Name, exportPVC.UID)
		}
		return nil, nil
	}

	if exportPVC.DeletionTimestamp == nil {
		if err := r.Client.Delete(ctx, exportPVC); err != nil && !kubeerrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to delete export claim %s: %w", names.ExportPVCName, err)
		}
		log.Printf("Recovery: export claim %s deleted", names.ExportPVCName)
	}

	return r.barrierExportPVCGone(ctx, names.ExportPVCName, exportPVC.UID)
}

// restoreSourcePVCBinding points the volume back at the claim it was taken from, confirms the binding by
// a fresh read (B4) and only then undoes the export-time changes to the volume: the reclaim policy and
// the takeover metadata. Doing it in that order keeps the volume protected by Retain for the whole
// window in which it is not yet provably back with its owner.
func (r *DataexportReconciler) restoreSourcePVCBinding(
	ctx context.Context,
	names Names,
	takeover takeoverRef,
) (*recoveryBarrier, error) {
	sourcePVC := takeover.SourceClaim

	pv := &corev1.PersistentVolume{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: takeover.PVName}, pv); err != nil {
		if kubeerrors.IsNotFound(err) {
			return &recoveryBarrier{Name: "B4", Object: client.ObjectKey{Name: takeover.PVName},
				Message: fmt.Sprintf("PV %s no longer exists; the volume cannot be returned", takeover.PVName)}, nil
		}
		return nil, fmt.Errorf("failed to get PV %s: %w", takeover.PVName, err)
	}
	if takeover.PVUID != "" && string(pv.UID) != takeover.PVUID {
		return &recoveryBarrier{Name: "B4", Object: client.ObjectKey{Name: pv.Name},
			Message: fmt.Sprintf("PV %s was replaced (UID %s, this export took over %s); refusing to rebind a volume that is not ours",
				pv.Name, pv.UID, takeover.PVUID)}, nil
	}

	// A volume that no longer carries our marker was already given back — the marker is the last thing a
	// completed restore removes. Re-running the restore on it would find no original reclaim policy to
	// put back and would condemn a perfectly healthy volume as inconsistent.
	if pv.Labels[dev1alpha1.LabelPVDataExporter] != "true" {
		log.Printf("Recovery: PV %s carries no export marker; the volume is already back with its owner", pv.Name)
		return nil, nil
	}

	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Client.Get(ctx, sourcePVC, claim); err != nil {
		if kubeerrors.IsNotFound(err) {
			return &recoveryBarrier{Name: "B4", Object: sourcePVC,
				Message: fmt.Sprintf("claim %s no longer exists; there is nobody to return PV %s to", sourcePVC, pv.Name)}, nil
		}
		return nil, fmt.Errorf("failed to get source claim %s: %w", sourcePVC, err)
	}
	// Who the volume was taken from, according to whichever witness has it: the record written before the
	// takeover, or the volume's own annotation, which is all a pre-UID export left behind.
	recordedSourceUID := takeover.SourcePVCUID
	if recordedSourceUID == "" {
		recordedSourceUID = pv.Annotations[dev1alpha1.AnnotationUserPVCUIDKey]
	}
	if recordedSourceUID != "" && string(claim.UID) != recordedSourceUID {
		return &recoveryBarrier{Name: "B4", Object: sourcePVC,
			Message: fmt.Sprintf("claim %s was recreated (UID %s, the volume was taken from %s); refusing to hand the volume to a different owner",
				sourcePVC, claim.UID, recordedSourceUID)}, nil
	}

	// What is left for the volume's annotations to contradict is not "a different owner" but "a different
	// export" or half a written identity. Neither resolves by waiting, so they are errors, not barriers.
	if err := checkRebindIdentity(pv, claim, rebindIdentityExpectation{DataExportUID: takeover.DataExportUID}); err != nil {
		return nil, err
	}

	if !claimRefPointsTo(pv.Spec.ClaimRef, sourcePVC, string(claim.UID)) {
		// The holder is re-checked immediately before the only irreversible write in the primitive: B3 was
		// evaluated against an earlier read, and a claim may have bound the volume since.
		exportClaim := types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: names.ExportPVCName}
		if classifyVolumeHolder(pv.Spec.ClaimRef, exportClaim, sourcePVC, takeover.ExportPVCUID) == holderStranger {
			return barrierStrangerHoldsTheVolume(pv), nil
		}

		updated := pv.DeepCopy()
		updated.Spec.ClaimRef = &corev1.ObjectReference{
			Namespace:       claim.Namespace,
			Name:            claim.Name,
			UID:             claim.UID,
			ResourceVersion: claim.ResourceVersion,
		}
		if err := r.Client.Patch(ctx, updated, client.MergeFromWithOptions(pv, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, fmt.Errorf("failed to rebind PV %s to %s: %w", pv.Name, sourcePVC, err)
		}
		log.Printf("Recovery: PV %s rebound to %s", pv.Name, sourcePVC)
		pv = updated
	}

	if blocked, err := r.barrierBindingRestored(ctx, pv.Name, sourcePVC, takeover.SourcePVCUID); err != nil || blocked != nil {
		return blocked, err
	}

	if err := r.restoreOriginalPVState(ctx, pv); err != nil {
		if errors.Is(err, ErrInvalidOriginalReclaimPolicy) {
			// Without the original policy the volume cannot be put back the way it was found, and guessing
			// could delete user data. Mark it so a human can see which volume needs attention.
			if labelErr := r.handleInconsistentPV(ctx, pv); labelErr != nil {
				return nil, fmt.Errorf("failed to label inconsistent PV %s: %w", pv.Name, labelErr)
			}
			return nil, fmt.Errorf("inconsistent PV %s: %w", pv.Name, err)
		}
		return nil, fmt.Errorf("failed to restore PV %s to its original state: %w", pv.Name, err)
	}
	return nil, nil
}

// restoreExportedPVMetadata puts a volume back the way it was found when no binding was ever taken away
// from anybody: the reclaim policy we changed and the marks we added.
func (r *DataexportReconciler) restoreExportedPVMetadata(ctx context.Context, pvName string) error {
	pv := &corev1.PersistentVolume{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get PV %s: %w", pvName, err)
	}
	if pv.Labels[dev1alpha1.LabelPVDataExporter] != "true" {
		return nil
	}
	if err := r.restoreOriginalPVState(ctx, pv); err != nil {
		return fmt.Errorf("failed to restore PV %s to its original state: %w", pvName, err)
	}
	return nil
}

func claimRefPointsTo(claimRef *corev1.ObjectReference, claim types.NamespacedName, uid string) bool {
	return claimRef != nil &&
		claimRef.Namespace == claim.Namespace &&
		claimRef.Name == claim.Name &&
		(uid == "" || string(claimRef.UID) == uid)
}

// cleanupExportInfrastructure removes what only ever served the export: the public Service and Ingress,
// the per-transfer CA Secret, and for snapshot-backed exports the VolumeRestoreRequest that provisioned
// the claim. None of it touches user-owned objects, so it is safe to repeat.
func (r *DataexportReconciler) cleanupExportInfrastructure(ctx context.Context, names Names) error {
	if names.TargetKindShort == dev1alpha1.KindSnapshotShort {
		if err := r.deleteVolumeRestoreRequest(ctx, nil, names); err != nil {
			return fmt.Errorf("failed to clear snapshot export VolumeRestoreRequest: %w", err)
		}
	}

	if _, err := publish.DeletePublicResources(
		ctx,
		r.Client,
		types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: names.HeadlessServiceName},
		types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: names.IngressResourceName},
	); err != nil {
		return err
	}

	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: names.CASecretName}, secret)
	switch {
	case kubeerrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("failed to get CA secret %s: %w", names.CASecretName, err)
	}
	if err := r.Client.Delete(ctx, secret); err != nil && !kubeerrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete CA secret %s: %w", names.CASecretName, err)
	}
	return nil
}

// releaseSourcePVC drops the export annotations and our finalizer from the user's claim. It runs last on
// purpose: that finalizer is what keeps the claim alive while its volume is still in our hands, so it may
// only go once everything else has been undone.
func (r *DataexportReconciler) releaseSourcePVC(ctx context.Context, sourcePVC types.NamespacedName) error {
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Client.Get(ctx, sourcePVC, claim); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get source claim %s: %w", sourcePVC, err)
	}
	return r.removeUserPVCExportingAnnotationsAndFinalizer(ctx, claim)
}
