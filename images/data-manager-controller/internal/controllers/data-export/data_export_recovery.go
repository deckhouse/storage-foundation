package dataexport

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	. "github.com/deckhouse/storage-foundation/common"
)

// takeoverStateKind classifies a PVC-backed export against the volume it borrowed. Three witnesses have
// to agree: what the export recorded when it took the volume, which claim exists now, and which claim the
// volume is actually bound to. Any two of them agreeing is not enough — that is precisely how a replaced
// claim or a replaced volume passes for healthy.
type takeoverStateKind int

const (
	// takeoverHealthy — the witnesses agree, or nothing has been taken over yet and provisioning may
	// proceed exactly as before.
	takeoverHealthy takeoverStateKind = iota
	// takeoverExportPVCLost — the export claim is gone while the volume is still bound to it.
	// Unrecoverable by re-creation (the binding pins the dead claim's UID), so the volume must be
	// returned.
	takeoverExportPVCLost
	// takeoverIdentityMismatch — the witnesses contradict each other: a claim carries our name but not
	// our identity, or the volume is held by a claim that is not the one we took it for. The export
	// serves nothing and the takeover still has to be undone.
	takeoverIdentityMismatch
	// takeoverLegacyLossUnprovable — the same loss on a takeover that predates the identity model. The
	// claim to restore cannot be proven, so the state is reported as blocked rather than acted upon.
	takeoverLegacyLossUnprovable
	// takeoverPVUnverified — the recorded volume is gone or was replaced under its name. Nothing may be
	// promised about a volume the export cannot even identify.
	takeoverPVUnverified
	// takeoverExportPVCUnproven — a claim occupies the export's generated name, but nothing shows this
	// export created it. It may be used for nothing: not to serve from, not to take a volume over for,
	// and not to delete.
	takeoverExportPVCUnproven
)

// takeoverState is a classification plus the sentence an operator will read on the object.
type takeoverState struct {
	kind    takeoverStateKind
	pvc     *corev1.PersistentVolumeClaim
	message string
}

// resolveTakeoverState reads the volume side of the takeover and classifies it together with the export
// claim the caller has already read. It is the only impure part: the classification itself is a pure
// function over the three witnesses, so the whole matrix is expressible in a table test.
func (r *DataexportReconciler) resolveTakeoverState(
	ctx context.Context,
	dataExport *dev1alpha1.DataExport,
	names Names,
	exportPVC *corev1.PersistentVolumeClaim,
) (takeoverState, error) {
	pv, err := r.findTakenOverPV(ctx, dataExport, names)
	if err != nil {
		return takeoverState{}, err
	}
	return classifyTakeoverState(dataExport, names, exportPVC, pv, r.Config.ControllerNamespace), nil
}

// classifyTakeoverState decides what state the export is in. It performs no writes and no reads:
// detection and mutation are separate passes, so the pass that discovers a loss cannot half-perform the
// recovery, and every branch is reachable from a test without a cluster.
//
// pv is the volume this export took over, or nil when none was found.
func classifyTakeoverState(
	dataExport *dev1alpha1.DataExport,
	names Names,
	exportPVC *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
	controllerNamespace string,
) takeoverState {
	recovery := dataExport.Status.Recovery

	// Nothing was ever taken over: a snapshot-backed export, or provisioning that has not reached the
	// rebind. A missing claim here is simply a claim to create.
	if recovery == nil && pv == nil {
		return healthyIfClaimProven(dataExport, names, exportPVC, pv)
	}

	if recovery != nil && recovery.PVName != "" {
		if pv == nil {
			return takeoverState{
				kind: takeoverPVUnverified,
				message: fmt.Sprintf("PV %s recorded as taken over from %s/%s no longer exists; the volume cannot be identified or returned",
					recovery.PVName, dataExport.Namespace, dataExport.Spec.TargetRef.Name),
			}
		}
		if recovery.PVUID != "" && string(pv.UID) != recovery.PVUID {
			return takeoverState{
				kind: takeoverPVUnverified,
				message: fmt.Sprintf("PV %s was replaced: this export took over UID %s, but the live volume has UID %s",
					pv.Name, recovery.PVUID, pv.UID),
			}
		}
	}

	// Past this point the volume is the one this export borrowed, so it is the authority on which claim
	// holds it. A record without a volume to check it against proves nothing on its own.
	if pv == nil {
		return healthyIfClaimProven(dataExport, names, exportPVC, pv)
	}

	claimRef := pv.Spec.ClaimRef
	holderIsOurClaim := claimRef != nil &&
		claimRef.Namespace == controllerNamespace &&
		claimRef.Name == names.ExportPVCName

	if !holderIsOurClaim {
		// Before the rebind the volume is still the user's and provisioning continues; the same shape
		// after a recorded takeover means the volume moved on without us.
		if recovery == nil {
			return healthyIfClaimProven(dataExport, names, exportPVC, pv)
		}
		return takeoverState{
			kind: takeoverIdentityMismatch,
			pvc:  exportPVC,
			message: fmt.Sprintf("PV %s is no longer held by export claim %s/%s: it is bound to %s",
				pv.Name, controllerNamespace, names.ExportPVCName, describeClaimRef(claimRef)),
		}
	}

	if recovery != nil && recovery.ExportPVCUID != "" && string(claimRef.UID) != recovery.ExportPVCUID {
		return takeoverState{
			kind: takeoverIdentityMismatch,
			pvc:  exportPVC,
			message: fmt.Sprintf("PV %s is bound to claim UID %s, but this export took the volume over for claim UID %s",
				pv.Name, claimRef.UID, recovery.ExportPVCUID),
		}
	}

	if exportPVC == nil {
		if recovery == nil {
			return takeoverState{
				kind: takeoverLegacyLossUnprovable,
				message: fmt.Sprintf("export claim %s is gone while PV %s is still bound to it, and this export records no takeover identity: "+
					"the volume cannot be returned automatically because the original claim cannot be proven",
					names.ExportPVCName, pv.Name),
			}
		}
		return takeoverState{
			kind: takeoverExportPVCLost,
			message: fmt.Sprintf("export claim %s is gone while PV %s is still bound to it; the claim cannot be recreated and the volume must be returned to %s/%s",
				names.ExportPVCName, pv.Name, dataExport.Namespace, dataExport.Spec.TargetRef.Name),
		}
	}

	// The claim exists and the volume is bound to our name — but not necessarily to this object. A claim
	// recreated under the same name leaves the volume bound to the dead one, so the export is serving
	// nothing even though every name matches.
	if string(exportPVC.UID) != string(claimRef.UID) {
		return takeoverState{
			kind: takeoverIdentityMismatch,
			pvc:  exportPVC,
			message: fmt.Sprintf("export claim %s/%s has UID %s, but PV %s is bound to UID %s: the live claim does not hold the volume",
				exportPVC.Namespace, exportPVC.Name, exportPVC.UID, pv.Name, claimRef.UID),
		}
	}

	if recovery != nil && recovery.ExportPVCUID != "" && string(exportPVC.UID) != recovery.ExportPVCUID {
		return takeoverState{
			kind: takeoverIdentityMismatch,
			pvc:  exportPVC,
			message: fmt.Sprintf("export claim %s/%s was replaced: the volume was taken over for UID %s, but the live claim has UID %s",
				exportPVC.Namespace, exportPVC.Name, recovery.ExportPVCUID, exportPVC.UID),
		}
	}

	return healthyIfClaimProven(dataExport, names, exportPVC, pv)
}

// healthyIfClaimProven is the healthy verdict, granted only if the claim the export is about to use can
// be shown to be its own. It guards the verdict rather than the classifier as a whole on purpose: a lost
// or replaced claim still has to be diagnosed and its volume still has to be given back, and only the
// right to *use* a claim depends on proving where it came from.
func healthyIfClaimProven(
	dataExport *dev1alpha1.DataExport,
	names Names,
	exportPVC *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
) takeoverState {
	if exportPVC == nil || claimBelongsToExport(dataExport, names, exportPVC, pv) {
		return takeoverState{kind: takeoverHealthy, pvc: exportPVC}
	}

	owner := exportPVC.Annotations[dev1alpha1.AnnotationDataExportUIDKey]
	message := fmt.Sprintf("export claim %s/%s carries no %s marker, so nothing shows this export created it",
		exportPVC.Namespace, exportPVC.Name, dev1alpha1.AnnotationDataExportUIDKey)
	if owner != "" {
		message = fmt.Sprintf("export claim %s/%s belongs to DataExport UID %s, not to this one (%s)",
			exportPVC.Namespace, exportPVC.Name, owner, dataExport.UID)
	}
	return takeoverState{
		kind:    takeoverExportPVCUnproven,
		pvc:     exportPVC,
		message: message + "; it will not be used, mutated or deleted",
	}
}

// claimBelongsToExport reports whether the live claim under the export's generated name can be shown to
// be the one this export created. The name itself is no evidence: it is derived from the export's own
// namespace and name, both of which a recreated object and a hand-made claim can carry too. Accepting it
// on the name alone is worse than an ordinary adoption, because the next step writes the stranger's UID
// into status.recovery, where it stops looking like an assumption and starts serving as proven identity
// for a takeover of the user's volume.
//
// Two witnesses can prove it, and each covers a stage the other cannot reach. Neither reads the claim's
// own opinion of itself except for the marker, which only this controller writes.
func claimBelongsToExport(
	dataExport *dev1alpha1.DataExport,
	names Names,
	exportPVC *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
) bool {
	// Snapshot exports are exempt on purpose, and NOT because the evidence is missing: a snapshot claim
	// is created by the external-provisioner from a VolumeRestoreRequest, and since the executor learned
	// to copy pvcTemplate.metadata onto the claim it created, new snapshot claims do carry the marker.
	//
	// The check stays off for one more rollout: claims made by the previous executor have no marker, and
	// enforcing here would strand every in-flight snapshot export on CleanupBlocked with no way back.
	// Turning it on and letting teardown delete only a proven claim is a single later change — the two
	// must ship together, or the teardown would start refusing to clean up the very claims this exemption
	// still lets the export use. See the resource-leak-protection design plan, "Происхождение export
	// claim" and §11, and step 7a of the P0 implementation plan.
	//
	// Do not "simplify" this by stamping the marker ourselves onto a claim found by name: that proves
	// nothing about where the claim came from, it only makes the adoption look proven.
	if names.TargetKindShort == dev1alpha1.KindSnapshotShort {
		return true
	}

	// The marker written at creation.
	if owner := exportPVC.Annotations[dev1alpha1.AnnotationDataExportUIDKey]; owner != "" {
		return owner == string(dataExport.UID)
	}

	// The volume vouches for it: this export took that volume over, and the volume is bound to this
	// claim by UID. It is what an export started before the marker existed has instead, and it is the
	// same evidence the recovery path already acts on. Before the rebind a legacy export has neither,
	// and stopping there is the intended price.
	//
	// The recorded takeover identity is deliberately not a third witness: a record that names a volume
	// this classifier could not verify is already reported as takeoverPVUnverified above, so trusting a
	// claim on the strength of the record alone would only ever apply where the volume itself is in
	// doubt.
	if pv == nil || pv.Spec.ClaimRef == nil {
		return false
	}
	return pv.Spec.ClaimRef.UID == exportPVC.UID &&
		pv.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] == dataExport.Namespace &&
		pv.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] == dataExport.Name
}

func describeClaimRef(claimRef *corev1.ObjectReference) string {
	if claimRef == nil {
		return "no claim"
	}
	return fmt.Sprintf("%s/%s (UID %s)", claimRef.Namespace, claimRef.Name, claimRef.UID)
}

// findTakenOverPV returns the PV this export took over, or nil if none was found. The recorded name is
// preferred because it is the identity written before the takeover; without a record (a pre-UID export)
// the PV is looked up the way the orphan sweep does, by our marker label and owner annotations. A
// recorded name that no longer resolves returns nil so the classifier can report it, rather than being
// mistaken for "nothing was taken over".
func (r *DataexportReconciler) findTakenOverPV(ctx context.Context, dataExport *dev1alpha1.DataExport, names Names) (*corev1.PersistentVolume, error) {
	if recovery := dataExport.Status.Recovery; recovery != nil && recovery.PVName != "" {
		pv := &corev1.PersistentVolume{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: recovery.PVName}, pv); err != nil {
			if kubeerrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("failed to get recorded PV %s: %w", recovery.PVName, err)
		}
		return pv, nil
	}

	// Snapshot-backed exports never take a user volume over, so there is nothing to look for.
	if names.TargetKindShort == dev1alpha1.KindSnapshotShort {
		return nil, nil
	}

	pvList := &corev1.PersistentVolumeList{}
	if err := r.Client.List(ctx, pvList, client.MatchingLabels{dev1alpha1.LabelPVDataExporter: "true"}); err != nil {
		return nil, fmt.Errorf("failed to list exported PVs: %w", err)
	}
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		if pv.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] == dataExport.Namespace &&
			pv.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] == dataExport.Name {
			return pv, nil
		}
	}
	return nil, nil
}

// applyManagedResourceFailure records a detected failure on the object. The Ready condition and the
// discriminator are set together, in memory, so the single deferred status write persists them
// atomically: an observer that saw the failure reason without the discriminator would read it as a
// recovery that had already finished and let the object settle as Failed with the volume still taken
// over.
//
// A blocked state deliberately writes no discriminator. It says the controller cannot safely act, not
// that it owes an action it is going to perform.
func applyManagedResourceFailure(dataExport *dev1alpha1.DataExport, state takeoverState) {
	var (
		reason        ConditionReason
		cleanupReason CleanupReason
	)

	switch state.kind {
	case takeoverExportPVCLost:
		reason, cleanupReason = ReasonManagedResourceLost, CleanupReasonExportPVCPostRebindLost
	case takeoverIdentityMismatch:
		reason, cleanupReason = ReasonManagedResourceIdentityMismatch, CleanupReasonExportPVCIdentityMismatch
	// No discriminator on any of these: the controller cannot prove a safe target, so it owes no action.
	// For an unproven claim that is the whole point — a discriminator would send the teardown after an
	// object that may belong to someone else, and the takeover it would be undoing never happened.
	case takeoverLegacyLossUnprovable, takeoverPVUnverified, takeoverExportPVCUnproven:
		reason = ReasonCleanupBlocked
	case takeoverHealthy:
		return
	}

	meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
		Type:               string(ConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             string(reason),
		Message:            state.message,
		ObservedGeneration: dataExport.Generation,
	})
	dataExport.Status.CleanupReason = string(cleanupReason)
}
