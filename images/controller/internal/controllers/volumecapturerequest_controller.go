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

package controllers

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "fox.flant.com/deckhouse/storage/storage-foundation/api/v1alpha1"
	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"

	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/config"
)

// Invariants (architectural guarantees):
//
// 1. Ownership model:
//    VCR (job, TTL) → ObjectKeeper (FollowObject, no TTL) → VSC/PV (artifacts)
//
// 2. VCR is NOT owner of VSC/PV:
//    - Only ObjectKeeper owns VSC/PV with controller=true
//    - VCR deletion triggers ObjectKeeper deletion (FollowObject)
//    - ObjectKeeper deletion triggers GC of VSC/PV through ownerRef
//
// 3. TTL is managed only for VCR:
//    - ObjectKeeper has no TTL (FollowObject mode)
//    - VCR TTL starts from CompletionTimestamp
//    - VCR deletion is safe: ObjectKeeper follows VCR lifecycle
//
// 4. No finalizer needed:
//    - ObjectKeeper follows VCR lifecycle and owns all artifacts
//    - Kubernetes GC handles cleanup automatically
//
// 5. PV can have only one controller owner:
//    - ObjectKeeper is the only controller owner of PV
//    - If another controller needs to own PV, it must coordinate with ObjectKeeper

// VolumeCaptureRequestController reconciles VolumeCaptureRequest objects
type VolumeCaptureRequestController struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.Options
}

func (r *VolumeCaptureRequestController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vcr", req.NamespacedName)

	var vcr storagev1alpha1.VolumeCaptureRequest
	if err := r.Get(ctx, req.NamespacedName, &vcr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		l.Error(err, "failed to get VolumeCaptureRequest")
		return ctrl.Result{}, err
	}

	// Skip if already Ready
	// NOTE: VCR is a fire-and-forget (one-time) operation. Once Ready, VCR will not be
	// reconciled again even if spec is changed. This is by design: VCR represents a single
	// snapshot capture operation. To create a new snapshot, create a new VCR.
	//
	// IMPORTANT: Temporary VolumeSnapshot (proxy-VS) cleanup happens in step 12 of processSnapshotMode
	// before VCR status is set to Ready. This ensures VS is deleted immediately upon VCR completion.
	// If VCR is already Ready, VS should already be cleaned up (or never existed).
	if isConditionTrue(vcr.Status.Conditions, ConditionTypeReady) {
		// Check TTL for completed VCR (after Ready short-circuit)
		// This ensures TTL cleanup happens only after VCR is fully completed
		if shouldDelete, requeueAfter, err := r.checkAndHandleTTL(ctx, &vcr); err != nil {
			l.Error(err, "Failed to check TTL")
			return ctrl.Result{}, err
		} else if shouldDelete {
			// Object was deleted, return
			return ctrl.Result{}, nil
		} else if requeueAfter > 0 {
			// TTL not expired yet, requeue
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		return ctrl.Result{}, nil
	}

	// Skip if already Failed and observed
	if isConditionFalse(vcr.Status.Conditions, ConditionTypeReady) {
		if vcr.Status.ObservedGeneration == vcr.Generation {
			// Check TTL for failed VCR (after Failed short-circuit)
			// This ensures TTL cleanup happens only after VCR is fully failed
			if shouldDelete, requeueAfter, err := r.checkAndHandleTTL(ctx, &vcr); err != nil {
				l.Error(err, "Failed to check TTL")
				return ctrl.Result{}, err
			} else if shouldDelete {
				// Object was deleted, return
				return ctrl.Result{}, nil
			} else if requeueAfter > 0 {
				// TTL not expired yet, requeue
				return ctrl.Result{RequeueAfter: requeueAfter}, nil
			}
			return ctrl.Result{}, nil
		}
	}

	// Handle deletion
	// NOTE: No finalizer needed.
	// ObjectKeeper follows VCR lifecycle and owns all artifacts.
	// When VCR is deleted:
	//   - ObjectKeeper is automatically deleted (FollowObject)
	//   - GC deletes VSC/PV through ownerRef
	if !vcr.DeletionTimestamp.IsZero() {
		l.Info("VCR is being deleted, skipping reconcile (ObjectKeeper will handle cleanup)")
		return ctrl.Result{}, nil
	}

	// Process based on mode
	switch vcr.Spec.Mode {
	case ModeSnapshot:
		return r.processSnapshotMode(ctx, &vcr)
	case ModeDetach:
		return r.processDetachMode(ctx, &vcr)
	default:
		base := vcr.DeepCopy()
		vcr.Status.ObservedGeneration = vcr.Generation
		now := metav1.Now()
		vcr.Status.CompletionTimestamp = &now
		message := fmt.Sprintf("Unknown mode: %s", vcr.Spec.Mode)
		setSingleCondition(&vcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             ErrorReasonInvalidMode,
			Message:            message,
			LastTransitionTime: now,
			ObservedGeneration: vcr.Generation,
		})
		// Set TTL annotation when marking as Failed (same Patch as Ready condition)
		// Use retry on conflict to handle concurrent updates
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &storagev1alpha1.VolumeCaptureRequest{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(&vcr), current); err != nil {
				return err
			}
			// Apply status changes
			current.Status = vcr.Status
			// Set TTL annotation
			r.setTTLAnnotation(current)
			// Patch both metadata (annotations) and status in the same operation
			return r.Patch(ctx, current, client.MergeFrom(base))
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update VCR with Failed condition and TTL annotation: %w", err)
		}
		return ctrl.Result{}, nil
	}
}

// processSnapshotMode handles Snapshot mode according to ADR:
// 1. VCR creates ObjectKeeper first (follows VCR, no TTL)
// 2. VCR creates VolumeSnapshotContent directly with ObjectKeeper as owner
// 3. external-snapshotter detects VSC and calls CSI CreateSnapshot
// 4. VCR waits for external-snapshotter to set ReadyToUse=true
// 5. VCR updates status with DataRef pointing to VSC
//
// According to ADR:
// - VCR creates VSC directly (VCR is the source of truth)
// - VCR does NOT create VolumeSnapshot (ADR forbids)
// - VCR does NOT set annotations on PVC (no snapshot-controller involvement)
// - OwnerRef chain: ObjectKeeper (controller=true) → VSC
// - VCR is NOT owner of VSC/PV - ObjectKeeper is the only owner
// - ObjectKeeper follows VCR lifecycle (automatically deleted when VCR is deleted)
// - TTL and request cleanup are handled by VCR controller, not ObjectKeeper
func (r *VolumeCaptureRequestController) processSnapshotMode(ctx context.Context, vcr *storagev1alpha1.VolumeCaptureRequest) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vcr", fmt.Sprintf("%s/%s", vcr.Namespace, vcr.Name), "mode", "Snapshot")

	// 1. Validate spec
	if vcr.Spec.PersistentVolumeClaimRef == nil {
		return r.markFailed(ctx, vcr, ErrorReasonInternalError, "PersistentVolumeClaimRef is required")
	}
	if vcr.Spec.VolumeSnapshotClassName == "" {
		return r.markFailed(ctx, vcr, ErrorReasonInternalError, "VolumeSnapshotClassName is required for Snapshot mode")
	}

	// 2. Check RBAC: try to get PVC (similar to MCR)
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: vcr.Spec.PersistentVolumeClaimRef.Namespace,
		Name:      vcr.Spec.PersistentVolumeClaimRef.Name,
	}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markFailed(ctx, vcr, ErrorReasonNotFound, fmt.Sprintf("PVC %s/%s not found", vcr.Spec.PersistentVolumeClaimRef.Namespace, vcr.Spec.PersistentVolumeClaimRef.Name))
		}
		if apierrors.IsForbidden(err) {
			return r.markFailed(ctx, vcr, ErrorReasonRBACDenied, fmt.Sprintf("Access denied to PVC %s/%s", vcr.Spec.PersistentVolumeClaimRef.Namespace, vcr.Spec.PersistentVolumeClaimRef.Name))
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PVC: %w", err)
	}

	// 3. Check if PVC is bound
	if pvc.Spec.VolumeName == "" {
		return r.markFailed(ctx, vcr, ErrorReasonInternalError, fmt.Sprintf("PVC %s/%s is not bound", pvc.Namespace, pvc.Name))
	}

	// 4. Get PV to extract volume handle
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markFailed(ctx, vcr, ErrorReasonNotFound, fmt.Sprintf("PV %s not found", pvc.Spec.VolumeName))
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PV: %w", err)
	}

	// Check if PV has CSI volume handle
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return r.markFailed(ctx, vcr, ErrorReasonInternalError, fmt.Sprintf("PV %s does not have CSI volume handle", pv.Name))
	}

	// 5. Validate VolumeSnapshotClass (needed for VSC creation)
	vscClass := &snapshotv1.VolumeSnapshotClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: vcr.Spec.VolumeSnapshotClassName}, vscClass); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markFailed(ctx, vcr, ErrorReasonNotFound, fmt.Sprintf("VolumeSnapshotClass %s not found", vcr.Spec.VolumeSnapshotClassName))
		}
		return ctrl.Result{}, fmt.Errorf("failed to get VolumeSnapshotClass: %w", err)
	}

	// Validate that VolumeSnapshotClass driver matches PV CSI driver
	if vscClass.Driver != pv.Spec.CSI.Driver {
		return r.markFailed(ctx, vcr, ErrorReasonInternalError,
			fmt.Sprintf("VolumeSnapshotClass driver %s does not match PV CSI driver %s", vscClass.Driver, pv.Spec.CSI.Driver))
	}

	// 6. Create ObjectKeeper FIRST - before creating VSC
	// ObjectKeeper is a pure ownership anchor that follows VCR lifecycle.
	// It owns resulting artifacts (VSC/PV) and is automatically deleted when VCR is deleted.
	// TTL and request cleanup are handled by VCR controller, not ObjectKeeper.
	csiVSCName := fmt.Sprintf("snapshot-%s", string(vcr.UID))
	retainerName := NamePrefixRetainer + csiVSCName
	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	err := r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper)
	if apierrors.IsNotFound(err) {
		objectKeeper = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: APIGroupDeckhouse,
				Kind:       KindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: retainerName,
			},
			Spec: deckhousev1alpha1.ObjectKeeperSpec{
				Mode: "FollowObject",
				FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
					APIVersion: APIGroupStorageDeckhouse,
					Kind:       KindVolumeCaptureRequest,
					Namespace:  vcr.Namespace,
					Name:       vcr.Name,
					UID:        string(vcr.UID),
				},
			},
		}
		if err := r.Create(ctx, objectKeeper); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create ObjectKeeper: %w", err)
		}
		l.Info("Created ObjectKeeper", "name", retainerName)
		// Re-read to get UID
		if err := r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get created ObjectKeeper: %w", err)
		}
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get ObjectKeeper: %w", err)
	} else {
		// ObjectKeeper exists - validate it belongs to this VCR
		// This protects against race conditions where VCR was deleted and recreated with same name
		if objectKeeper.Spec.FollowObjectRef == nil {
			return ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has no FollowObjectRef", retainerName)
		}
		if objectKeeper.Spec.FollowObjectRef.UID != string(vcr.UID) {
			return ctrl.Result{}, fmt.Errorf("ObjectKeeper %s belongs to another VCR (UID mismatch: expected %s, got %s)",
				retainerName, string(vcr.UID), objectKeeper.Spec.FollowObjectRef.UID)
		}
	}

	// 7. Create VolumeSnapshotContent directly (VCR is the source of truth)
	// According to ADR: VCR creates VSC directly, no snapshot-controller involvement
	// Check if VSC already exists (idempotency - don't recreate)
	csiVSC := &snapshotv1.VolumeSnapshotContent{}
	err = r.Get(ctx, client.ObjectKey{Name: csiVSCName}, csiVSC)
	if apierrors.IsNotFound(err) {
		// Create VSC with proper spec and ownerRefs
		volumeHandle := pv.Spec.CSI.VolumeHandle
		vscClassName := vcr.Spec.VolumeSnapshotClassName
		controllerTrue := true

		csiVSC = &snapshotv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name: csiVSCName,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: APIGroupDeckhouse,
						Kind:       KindObjectKeeper,
						Name:       retainerName,
						UID:        objectKeeper.UID,
						Controller: &controllerTrue, // ObjectKeeper is the only owner and controller
					},
				},
			},
			Spec: snapshotv1.VolumeSnapshotContentSpec{
				Driver:                  vscClass.Driver,
				VolumeSnapshotClassName: &vscClassName,
				DeletionPolicy:          vscClass.DeletionPolicy,
				Source: snapshotv1.VolumeSnapshotContentSource{
					VolumeHandle: &volumeHandle,
				},
				// ADR forbids VolumeSnapshot - volumeSnapshotRef must be empty
				VolumeSnapshotRef: corev1.ObjectReference{},
			},
		}
		if err := r.Create(ctx, csiVSC); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create VolumeSnapshotContent: %w", err)
		}
		l.Info("Created VolumeSnapshotContent", "name", csiVSCName)
		// Re-read VSC to get latest state (including UID) before checking ReadyToUse
		if err := r.Get(ctx, client.ObjectKey{Name: csiVSCName}, csiVSC); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get created VolumeSnapshotContent: %w", err)
		}
		// Continue to check ReadyToUse below (don't requeue immediately - check status first)
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get VolumeSnapshotContent: %w", err)
	}
	// VSC exists - continue to wait for ReadyToUse (idempotency)

	// 8. Wait for external-snapshotter to set ReadyToUse=true on VSC
	// external-snapshotter will detect VSC, call CSI CreateSnapshot, and set ReadyToUse when snapshot is ready
	if csiVSC.Status == nil || csiVSC.Status.ReadyToUse == nil || !*csiVSC.Status.ReadyToUse {
		// Check for CSI errors (e.g., broken driver, invalid parameters)
		if csiVSC.Status != nil && csiVSC.Status.Error != nil {
			errorMsg := "unknown error"
			if csiVSC.Status.Error.Message != nil {
				errorMsg = *csiVSC.Status.Error.Message
			}
			return r.markFailed(ctx, vcr, ErrorReasonInternalError,
				fmt.Sprintf("CSI snapshot creation failed: %s", errorMsg))
		}
		l.Info("Waiting for external-snapshotter to set ReadyToUse=true", "name", csiVSCName)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// 9. Update VCR status (VSC is ready)
	vcrBase := vcr.DeepCopy()
	vcr.Status.DataRef = &storagev1alpha1.ObjectReference{
		Name: csiVSCName,
		Kind: "VolumeSnapshotContent", // Explicitly set Kind according to ADR
	}
	vcr.Status.ObservedGeneration = vcr.Generation
	now := metav1.Now()
	vcr.Status.CompletionTimestamp = &now
	setSingleCondition(&vcr.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             ConditionReasonCompleted,
		Message:            fmt.Sprintf("CSI VolumeSnapshotContent %s ready", csiVSCName),
		LastTransitionTime: now,
		ObservedGeneration: vcr.Generation,
	})
	// Set TTL annotation when marking as Ready (same Patch as Ready condition)
	// Use retry on conflict to handle concurrent updates
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeCaptureRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); err != nil {
			return err
		}
		// Apply status changes
		current.Status = vcr.Status
		// Set TTL annotation
		r.setTTLAnnotation(current)
		// Patch both metadata (annotations) and status in the same operation
		return r.Patch(ctx, current, client.MergeFrom(vcrBase))
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update VCR with Ready condition and TTL annotation: %w", err)
	}

	l.Info("VolumeCaptureRequest completed", "mode", "Snapshot", "csiVSC", csiVSCName)
	return ctrl.Result{}, nil
}

// processDetachMode handles Detach mode: detaches PV from PVC
// According to ADR, PV after Detach gets annotation storage.deckhouse.io/detached: "true"
// Provisioner ignores such PV for normal binding (quarantine PV).
func (r *VolumeCaptureRequestController) processDetachMode(ctx context.Context, vcr *storagev1alpha1.VolumeCaptureRequest) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vcr", fmt.Sprintf("%s/%s", vcr.Namespace, vcr.Name), "mode", "Detach")

	// 1. Validate spec
	if vcr.Spec.PersistentVolumeClaimRef == nil {
		return r.markFailed(ctx, vcr, ErrorReasonInternalError, "PersistentVolumeClaimRef is required")
	}

	// 2. Check RBAC: try to get PVC
	// NOTE: If PVC is already deleted (from previous reconcile), we need to get PV from annotation or VCR status
	pvc := &corev1.PersistentVolumeClaim{}
	var pv *corev1.PersistentVolume
	pvcNotFound := false
	pvNameFromAnnotation := ""
	if vcr.Annotations != nil {
		if pvName, ok := vcr.Annotations["storage.deckhouse.io/detach-pv-name"]; ok {
			pvNameFromAnnotation = pvName
		}
	}

	if err := r.Get(ctx, client.ObjectKey{
		Namespace: vcr.Spec.PersistentVolumeClaimRef.Namespace,
		Name:      vcr.Spec.PersistentVolumeClaimRef.Name,
	}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			// PVC already deleted - this is expected after first reconcile
			pvcNotFound = true
			l.Info("PVC already deleted, proceeding to detach PV", "pvc", fmt.Sprintf("%s/%s", vcr.Spec.PersistentVolumeClaimRef.Namespace, vcr.Spec.PersistentVolumeClaimRef.Name))
			// Try to get PV from annotation or VCR status
			var pvName string
			if pvNameFromAnnotation != "" {
				pvName = pvNameFromAnnotation
			} else if vcr.Status.DataRef != nil && vcr.Status.DataRef.Kind == "PersistentVolume" {
				pvName = vcr.Status.DataRef.Name
			} else {
				// Cannot proceed without PV name
				return ctrl.Result{}, fmt.Errorf("PVC deleted but cannot determine PV name (no annotation or status)")
			}
			pv = &corev1.PersistentVolume{}
			if err := r.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get PV %s: %w", pvName, err)
			}
		} else if apierrors.IsForbidden(err) {
			return r.markFailed(ctx, vcr, ErrorReasonRBACDenied, fmt.Sprintf("Access denied to PVC %s/%s", vcr.Spec.PersistentVolumeClaimRef.Namespace, vcr.Spec.PersistentVolumeClaimRef.Name))
		} else {
			return ctrl.Result{}, fmt.Errorf("failed to get PVC: %w", err)
		}
	}

	if !pvcNotFound {
		// 3. Check if PVC is bound
		if pvc.Spec.VolumeName == "" {
			return r.markFailed(ctx, vcr, ErrorReasonInternalError, fmt.Sprintf("PVC %s/%s is not bound", pvc.Namespace, pvc.Name))
		}

		// 4. Get PV and store its name in annotation for future reconciles
		pv = &corev1.PersistentVolume{}
		if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
			if apierrors.IsNotFound(err) {
				return r.markFailed(ctx, vcr, ErrorReasonNotFound, fmt.Sprintf("PV %s not found", pvc.Spec.VolumeName))
			}
			return ctrl.Result{}, fmt.Errorf("failed to get PV: %w", err)
		}

		// Store PV name in annotation for future reconciles (when PVC is deleted)
		if vcr.Annotations == nil {
			vcr.Annotations = make(map[string]string)
		}
		if vcr.Annotations["storage.deckhouse.io/detach-pv-name"] != pv.Name {
			vcr.Annotations["storage.deckhouse.io/detach-pv-name"] = pv.Name
			if err := r.Update(ctx, vcr); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update VCR annotation: %w", err)
			}
		}
	}

	// 5. Check if PV is already detached
	// NOTE: If PV.Spec.PersistentVolumeReclaimPolicy != Retain, detaching may lead to
	// PV deletion by kube-controller-manager. This is a known limitation and should be
	// documented for users. In production, PV should have ReclaimPolicy=Retain for Detach mode.
	// Note: We check both ClaimRef and annotation to ensure PV was detached by our controller
	if pv.Spec.ClaimRef == nil {
		// Check if PV has our detached annotation to confirm it was detached by VCR
		alreadyDetachedByVCR := false
		if pv.Annotations != nil {
			if val, ok := pv.Annotations["storage.deckhouse.io/detached"]; ok && val == "true" {
				alreadyDetachedByVCR = true
			}
		}
		if !alreadyDetachedByVCR {
			// PV was detached by someone else, not by VCR - this is unexpected
			l.Info("PV already detached but not by VCR", "pv", pv.Name)
		}
		// Already detached, update status
		base := vcr.DeepCopy()
		vcr.Status.DataRef = &storagev1alpha1.ObjectReference{
			Name: pv.Name,
			Kind: "PersistentVolume",
		}
		vcr.Status.ObservedGeneration = vcr.Generation
		now := metav1.Now()
		vcr.Status.CompletionTimestamp = &now
		setSingleCondition(&vcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             ConditionReasonCompleted,
			Message:            fmt.Sprintf("PV %s already detached", pv.Name),
			LastTransitionTime: now,
			ObservedGeneration: vcr.Generation,
		})
		// Set TTL annotation when marking as Ready (same Patch as Ready condition)
		// Use retry on conflict to handle concurrent updates
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &storagev1alpha1.VolumeCaptureRequest{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); err != nil {
				return err
			}
			// Apply status changes
			current.Status = vcr.Status
			// Set TTL annotation
			r.setTTLAnnotation(current)
			// Patch both metadata (annotations) and status in the same operation
			return r.Patch(ctx, current, client.MergeFrom(base))
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update VCR with Ready condition and TTL annotation: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// 6. Delete PVC according to ADR (only if PVC still exists)
	// According to ADR, Detach mode should delete PVC
	// PV will be retained due to ReclaimPolicy=Retain
	if !pvcNotFound {
		// Check if PVC is already deleted
		pvcToDelete := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: pvc.Namespace, Name: pvc.Name}, pvcToDelete); err != nil {
			if apierrors.IsNotFound(err) {
				// PVC already deleted, continue
				l.Info("PVC already deleted", "pvc", fmt.Sprintf("%s/%s", pvc.Namespace, pvc.Name))
			} else {
				return ctrl.Result{}, fmt.Errorf("failed to check PVC status: %w", err)
			}
		} else {
			// PVC still exists, delete it
			if err := r.Delete(ctx, pvcToDelete); err != nil {
				if apierrors.IsNotFound(err) {
					// PVC was deleted between Get and Delete, continue
					l.Info("PVC was deleted concurrently", "pvc", fmt.Sprintf("%s/%s", pvc.Namespace, pvc.Name))
				} else {
					return ctrl.Result{}, fmt.Errorf("failed to delete PVC: %w", err)
				}
			} else {
				l.Info("Deleted PVC", "pvc", fmt.Sprintf("%s/%s", pvc.Namespace, pvc.Name))
				// Wait for PVC deletion to complete before proceeding
				// This ensures PV ClaimRef can be safely removed
				return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
			}
		}
	}

	// 7. Detach PV from PVC (remove claimRef)
	// According to ADR, PV after Detach gets annotation storage.deckhouse.io/detached: "true"
	// NOTE: PV should have ReclaimPolicy=Retain to prevent accidental deletion
	// Re-read PV to get latest state before patching
	updatedPV := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pv.Name}, updatedPV); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to re-read PV before detach: %w", err)
	}
	base := updatedPV.DeepCopy()
	updatedPV.Spec.ClaimRef = nil
	if updatedPV.Annotations == nil {
		updatedPV.Annotations = make(map[string]string)
	}
	updatedPV.Annotations["storage.deckhouse.io/detached"] = "true"

	if err := r.Patch(ctx, updatedPV, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to detach PV: %w", err)
	}
	l.Info("Detached PV from PVC", "pv", updatedPV.Name)

	// 8. Create ObjectKeeper
	// ObjectKeeper is a pure ownership anchor that follows VCR lifecycle.
	// It owns resulting artifacts (VSC/PV) and is automatically deleted when VCR is deleted.
	// TTL and request cleanup are handled by VCR controller, not ObjectKeeper.
	retainerName := NamePrefixRetainerPV + string(vcr.UID)
	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	err := r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper)
	if apierrors.IsNotFound(err) {
		objectKeeper = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: APIGroupDeckhouse,
				Kind:       KindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: retainerName,
			},
			Spec: deckhousev1alpha1.ObjectKeeperSpec{
				Mode: "FollowObject",
				FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
					APIVersion: APIGroupStorageDeckhouse,
					Kind:       KindVolumeCaptureRequest,
					Namespace:  vcr.Namespace,
					Name:       vcr.Name,
					UID:        string(vcr.UID),
				},
			},
		}
		if err := r.Create(ctx, objectKeeper); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create ObjectKeeper: %w", err)
		}
		l.Info("Created ObjectKeeper", "name", retainerName)
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get ObjectKeeper: %w", err)
	} else {
		// ObjectKeeper exists - validate it belongs to this VCR
		// This protects against race conditions where VCR was deleted and recreated with same name
		if objectKeeper.Spec.FollowObjectRef == nil {
			return ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has no FollowObjectRef", retainerName)
		}
		if objectKeeper.Spec.FollowObjectRef.UID != string(vcr.UID) {
			return ctrl.Result{}, fmt.Errorf("ObjectKeeper %s belongs to another VCR (UID mismatch: expected %s, got %s)",
				retainerName, string(vcr.UID), objectKeeper.Spec.FollowObjectRef.UID)
		}
	}

	// 9. Set ownerRef on PV: ObjectKeeper → PV
	// INVARIANT: PV can have only one controller owner - ObjectKeeper.
	// If another controller needs to own PV, it must coordinate with ObjectKeeper.
	// Note: We append ownerRef instead of replacing to preserve existing ownerRefs
	pvBase := pv.DeepCopy()
	// Check if ObjectKeeper ownerRef already exists (with UID check to prevent conflicts)
	retainerOwnerRefExists := false
	for _, ref := range pv.OwnerReferences {
		if ref.Kind == KindObjectKeeper && ref.Name == retainerName && ref.UID == objectKeeper.UID {
			retainerOwnerRefExists = true
			break
		}
	}
	if !retainerOwnerRefExists {
		pv.OwnerReferences = append(pv.OwnerReferences, metav1.OwnerReference{
			APIVersion: APIGroupDeckhouse,
			Kind:       KindObjectKeeper,
			Name:       retainerName,
			UID:        objectKeeper.UID,
			Controller: func() *bool { b := true; return &b }(),
		})
	}
	if err := r.Patch(ctx, pv, client.MergeFrom(pvBase)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set ownerRef on PV: %w", err)
	}

	// 10. Update VCR status
	vcrBase := vcr.DeepCopy()
	vcr.Status.DataRef = &storagev1alpha1.ObjectReference{
		Name: pv.Name,
		Kind: "PersistentVolume", // Explicitly set Kind according to ADR
	}
	vcr.Status.ObservedGeneration = vcr.Generation
	now := metav1.Now()
	vcr.Status.CompletionTimestamp = &now
	setSingleCondition(&vcr.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             ConditionReasonCompleted,
		Message:            fmt.Sprintf("PV %s detached", pv.Name),
		LastTransitionTime: now,
		ObservedGeneration: vcr.Generation,
	})
	// Set TTL annotation when marking as Ready (same Patch as Ready condition)
	// Use retry on conflict to handle concurrent updates
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeCaptureRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); err != nil {
			return err
		}
		// Apply status changes
		current.Status = vcr.Status
		// Set TTL annotation
		r.setTTLAnnotation(current)
		// Patch both metadata (annotations) and status in the same operation
		return r.Patch(ctx, current, client.MergeFrom(vcrBase))
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update VCR with Ready condition and TTL annotation: %w", err)
	}

	l.Info("VolumeCaptureRequest completed", "mode", "Detach", "pv", pv.Name)
	return ctrl.Result{}, nil
}

// Helper functions

// setTTLAnnotation sets TTL annotation on the object
// TTL is set when Ready/Failed condition is set, and used for automatic deletion
// TTL comes from configuration (storage-foundation module settings), not from VCR spec
// If annotation already exists, it is not overwritten (idempotent)
func (r *VolumeCaptureRequestController) setTTLAnnotation(vcr *storagev1alpha1.VolumeCaptureRequest) {
	// Don't overwrite if annotation already exists
	if vcr.Annotations != nil {
		if _, exists := vcr.Annotations[AnnotationKeyTTL]; exists {
			return
		}
	}
	if vcr.Annotations == nil {
		vcr.Annotations = make(map[string]string)
	}
	// Get TTL from configuration (default: 10m)
	ttlStr := config.DefaultRequestTTLStr
	if r.Config != nil && r.Config.RequestTTLStr != "" {
		ttlStr = r.Config.RequestTTLStr
	}
	vcr.Annotations[AnnotationKeyTTL] = ttlStr
}

// checkAndHandleTTL checks if TTL has expired and deletes the object if needed
// Returns (shouldDelete, requeueAfter, error)
func (r *VolumeCaptureRequestController) checkAndHandleTTL(ctx context.Context, vcr *storagev1alpha1.VolumeCaptureRequest) (bool, time.Duration, error) {
	// Check if TTL annotation exists
	ttlStr, hasTTL := vcr.Annotations[AnnotationKeyTTL]
	if !hasTTL {
		// No TTL annotation, nothing to do
		return false, 0, nil
	}

	// Parse TTL duration
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		// Invalid TTL format - set Ready=False condition to inform user
		l := log.FromContext(ctx)
		l.Error(err, "Invalid TTL annotation format", "ttl", ttlStr)

		// Set Ready=False condition to inform user about TTL issue
		base := vcr.DeepCopy()
		vcr.Status.ObservedGeneration = vcr.Generation
		now := metav1.Now()
		setSingleCondition(&vcr.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             ConditionReasonInvalidTTL,
			Message:            fmt.Sprintf("Invalid TTL annotation format: %s (error: %v)", ttlStr, err),
			LastTransitionTime: now,
			ObservedGeneration: vcr.Generation,
		})
		// Use retry on conflict to handle concurrent updates
		if patchErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &storagev1alpha1.VolumeCaptureRequest{}
			if getErr := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); getErr != nil {
				return getErr
			}
			current.Status = vcr.Status
			return r.Status().Patch(ctx, current, client.MergeFrom(base))
		}); patchErr != nil {
			l.Error(patchErr, "Failed to update status with InvalidTTL condition")
			return false, 0, patchErr
		}
		return false, 0, nil
	}

	// Calculate expiration time: completionTimestamp + TTL
	if vcr.Status.CompletionTimestamp == nil {
		// No completion timestamp yet, TTL hasn't started
		return false, 0, nil
	}

	completionTime := vcr.Status.CompletionTimestamp.Time
	expirationTime := completionTime.Add(ttl)
	now := time.Now()

	// Check if TTL has expired
	if now.After(expirationTime) {
		// TTL expired, delete the object
		// NOTE: VCR deletion is safe: ObjectKeeper follows VCR lifecycle.
		// When VCR is deleted, ObjectKeeper is automatically deleted (follows object).
		// When ObjectKeeper is deleted, GC deletes VSC/PV through ownerRef.
		log.FromContext(ctx).Info("TTL expired, deleting VolumeCaptureRequest",
			"namespace", vcr.Namespace,
			"name", vcr.Name,
			"completionTime", completionTime,
			"expirationTime", expirationTime,
		)
		if err := r.Delete(ctx, vcr); err != nil {
			if apierrors.IsNotFound(err) {
				// Already deleted, that's fine (double-delete is safe)
				return true, 0, nil
			}
			return false, 0, fmt.Errorf("failed to delete expired VolumeCaptureRequest: %w", err)
		}
		return true, 0, nil
	}

	// TTL not expired yet, calculate requeue time with jitter to avoid reconcile flood
	timeUntilExpiration := expirationTime.Sub(now)
	// Requeue after min(timeLeft, 1m), but not less than 30s
	requeueAfter := timeUntilExpiration
	if requeueAfter > time.Minute {
		requeueAfter = time.Minute
	}
	if requeueAfter < 30*time.Second {
		requeueAfter = 30 * time.Second
	}

	// Add jitter (±10%) to avoid reconcile flood when multiple VCR expire simultaneously
	// This follows the pattern used by JobController, DeploymentController, etc.
	jitterRange := requeueAfter / 10 // 10% jitter
	jitter := time.Duration(rand.Int63n(int64(2*jitterRange))) - jitterRange
	requeueAfter = requeueAfter + jitter
	if requeueAfter < 30*time.Second {
		requeueAfter = 30 * time.Second // Ensure minimum after jitter
	}

	return false, requeueAfter, nil
}

func (r *VolumeCaptureRequestController) markFailed(ctx context.Context, vcr *storagev1alpha1.VolumeCaptureRequest, reason, message string) (ctrl.Result, error) {
	vcrBase := vcr.DeepCopy()
	vcr.Status.ObservedGeneration = vcr.Generation
	now := metav1.Now()
	vcr.Status.CompletionTimestamp = &now
	setSingleCondition(&vcr.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: vcr.Generation,
	})
	// Set TTL annotation when marking as Failed (same Patch as Ready condition)
	// Use retry on conflict to handle concurrent updates
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeCaptureRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); err != nil {
			return err
		}
		// Apply status changes
		current.Status = vcr.Status
		// Set TTL annotation
		r.setTTLAnnotation(current)
		// Patch both metadata (annotations) and status in the same operation
		return r.Patch(ctx, current, client.MergeFrom(vcrBase))
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update VCR with Failed condition and TTL annotation: %w", err)
	}
	return ctrl.Result{}, nil
}

// Helper functions for future use (e.g., VRR, metrics, reporting)
// Currently unused but kept for potential future needs

// getVolumeMode returns the volume mode of PVC (Block or Filesystem)
func (r *VolumeCaptureRequestController) getVolumeMode(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return "Block"
	}
	return "Filesystem"
}

// getSize returns the size of PVC as a string
func (r *VolumeCaptureRequestController) getSize(pvc *corev1.PersistentVolumeClaim) string {
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		return q.String()
	}
	return "1Gi"
}

// getSizeBytes returns the size of snapshot in bytes from VolumeSnapshot status
// NOTE: Currently unused - kept for potential future use when migrating to VSC-only workflow
// or when implementing additional status reporting features
func (r *VolumeCaptureRequestController) getSizeBytes(csiVS *snapshotv1.VolumeSnapshot) int64 {
	// Get size from CSI VolumeSnapshot (RestoreSize is resource.Quantity)
	if csiVS.Status != nil && csiVS.Status.RestoreSize != nil {
		return csiVS.Status.RestoreSize.Value()
	}
	return 0
}

// getStorageSnapshotID returns the snapshot handle from bound VolumeSnapshotContent
// NOTE: Currently unused - kept for potential future use when migrating to VSC-only workflow
// or when implementing additional status reporting features
func (r *VolumeCaptureRequestController) getStorageSnapshotID(csiVS *snapshotv1.VolumeSnapshot) string {
	// Get snapshot handle from bound VolumeSnapshotContent
	if csiVS.Status != nil && csiVS.Status.BoundVolumeSnapshotContentName != nil {
		// The content name is the storage snapshot ID
		return *csiVS.Status.BoundVolumeSnapshotContentName
	}
	return ""
}

func (r *VolumeCaptureRequestController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.VolumeCaptureRequest{}).
		Complete(r)
}
