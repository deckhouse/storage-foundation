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
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/controller/pkg/config"
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
//
// DESIGN NOTE:
//
// VolumeCaptureRequestController intentionally relies only on informer cache.
// Read-after-write consistency is NOT guaranteed and NOT required.
// Cluster configuration objects (e.g. VolumeSnapshotClass) must exist
// before creating VolumeCaptureRequest.
//
// Any misconfiguration is treated as terminal failure.
//
// StorageClass is read via APIReader (direct API, no cache) since it's cluster-level
// configuration that doesn't need to be watched or cached.
//
// Controllers MUST read StorageClass via APIReader. APIReader is a required dependency.
type VolumeCaptureRequestController struct {
	client.Client
	APIReader client.Reader // Required: for reading StorageClass directly from API server
	Scheme    *runtime.Scheme
	Config    *config.Options
}

func (r *VolumeCaptureRequestController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vcr", req.NamespacedName)
	l.Info("Reconciling VolumeCaptureRequest")

	var vcr storagev1alpha1.VolumeCaptureRequest
	if err := r.Get(ctx, req.NamespacedName, &vcr); err != nil {
		if apierrors.IsNotFound(err) {
			l.V(1).Info("VolumeCaptureRequest not found, skipping")
			return ctrl.Result{}, nil
		}
		l.Error(err, "failed to get VolumeCaptureRequest")
		return ctrl.Result{}, err
	}

	// Skip if terminal (Ready=True or Ready=False)
	// NOTE: VCR is a fire-and-forget (one-time) operation. Once terminal, VCR will not be
	// reconciled again even if spec is changed. This is by design: VCR represents a single
	// snapshot capture operation. To create a new snapshot, create a new VCR.
	//
	// IMPORTANT: Temporary VolumeSnapshot (proxy-VS) cleanup happens in step 12 of processSnapshotMode
	// before VCR status is set to Ready. This ensures VS is deleted immediately upon VCR completion.
	// If VCR is already Ready, VS should already be cleaned up (or never existed).
	if isVolumeCaptureTerminal(vcr.Status.Conditions) {
		// VCR is terminal - TTL cleanup is handled by background TTL scanner, not in reconcile loop
		// This ensures reconcile loop doesn't block on TTL checks
		l.V(1).Info("VCR is terminal, skipping reconcile")
		return ctrl.Result{}, nil
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
	l.Info("Processing VCR", "mode", vcr.Spec.Mode)
	switch vcr.Spec.Mode {
	case ModeSnapshot:
		return r.processSnapshotMode(ctx, &vcr)
	case ModeDetach:
		return r.processDetachMode(ctx, &vcr)
	default:
		l.Error(nil, "Unknown mode", "mode", vcr.Spec.Mode)
		if err := r.finalizeVCR(ctx, &vcr, metav1.ConditionFalse, storagev1alpha1.ConditionReasonInvalidMode, fmt.Sprintf("Unknown mode: %s", vcr.Spec.Mode)); err != nil {
			return ctrl.Result{}, err
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
//
// INVARIANT:
// - VCR creates VSC exactly once
// - Any error in VSC.status.error is terminal
// - VCR never retries snapshot creation
func (r *VolumeCaptureRequestController) processSnapshotMode(ctx context.Context, vcr *storagev1alpha1.VolumeCaptureRequest) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vcr", fmt.Sprintf("%s/%s", vcr.Namespace, vcr.Name), "mode", "Snapshot")
	l.Info("Processing VolumeCaptureRequest in Snapshot mode")

	target, err := requireCaptureTarget(vcr.Spec)
	if err != nil {
		l.Error(err, "Invalid spec.target")
		return r.markFailed(ctx, vcr, storagev1alpha1.ConditionReasonInternalError, err.Error())
	}
	// spec.target carries no namespace; the captured PVC always lives in the VCR namespace.
	// Re-inject it so the per-target capture path and the status binding both see the namespace.
	target.Namespace = vcr.Namespace

	retainerName := objectKeeperNameForVCR(vcr.UID)
	_, result, err := r.ensureObjectKeeper(ctx, retainerName, vcr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.RequeueAfter > 0 {
		return result, nil
	}

	tr, targetResult, err := r.processSnapshotTarget(ctx, vcr, retainerName, target)
	if err != nil {
		return ctrl.Result{}, err
	}

	if tr.terminal != nil {
		l.Error(nil, "Target capture failed", "targetUID", tr.terminal.target.UID, "reason", tr.terminal.reason)
		return r.markFailedSnapshotForTarget(ctx, vcr, tr.terminal.target, tr.terminal.vscName, tr.terminal.vscUID, tr.terminal.reason, tr.terminal.message)
	}

	if tr.ready && tr.binding != nil {
		vcr.Status.Data = tr.binding
		if err := r.finalizeVCR(ctx, vcr, metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted, "target ready"); err != nil {
			return ctrl.Result{}, err
		}
		l.Info("VolumeCaptureRequest completed", "mode", "Snapshot")
		return ctrl.Result{}, nil
	}

	// Still capturing: surface progress and requeue.
	if err := r.patchVCRSnapshotPending(ctx, vcr); err != nil {
		return ctrl.Result{}, err
	}
	requeueAfter := targetResult.RequeueAfter
	if requeueAfter == 0 {
		requeueAfter = 5 * time.Second
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// processDetachMode handles Detach mode: detaches PV from PVC
// According to ADR, PV after Detach gets annotation storage-foundation.deckhouse.io/detached: "true"
// Provisioner ignores such PV for normal binding (quarantine PV).
func (r *VolumeCaptureRequestController) processDetachMode(ctx context.Context, vcr *storagev1alpha1.VolumeCaptureRequest) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vcr", fmt.Sprintf("%s/%s", vcr.Namespace, vcr.Name), "mode", "Detach")

	// 1. Validate spec (single target)
	target, err := requireCaptureTarget(vcr.Spec)
	if err != nil {
		return r.markFailed(ctx, vcr, storagev1alpha1.ConditionReasonInternalError, err.Error())
	}
	// spec.target carries no namespace; the captured PVC always lives in the VCR namespace.
	target.Namespace = vcr.Namespace

	// 2. Check RBAC: try to get PVC
	// NOTE: If PVC is already deleted (from previous reconcile), we need to get PV from annotation or VCR status
	pvc := &corev1.PersistentVolumeClaim{}
	var pv *corev1.PersistentVolume
	pvcNotFound := false
	pvNameFromAnnotation := ""
	if vcr.Annotations != nil {
		if pvName, ok := vcr.Annotations["storage-foundation.deckhouse.io/detach-pv-name"]; ok {
			pvNameFromAnnotation = pvName
		}
	}

	if err := r.Get(ctx, client.ObjectKey{
		Namespace: target.Namespace,
		Name:      target.Name,
	}, pvc); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			// PVC already deleted - this is expected after first reconcile
			pvcNotFound = true
			l.Info("PVC already deleted, proceeding to detach PV", "pvc", fmt.Sprintf("%s/%s", target.Namespace, target.Name))
			// Try to get PV from annotation or VCR status
			var pvName string
			if pvNameFromAnnotation != "" {
				pvName = pvNameFromAnnotation
			} else if artifact, ok := dataArtifactRef(vcr.Status); ok && artifact.Kind == "PersistentVolume" {
				pvName = artifact.Name
			} else {
				// Cannot proceed without PV name
				return ctrl.Result{}, fmt.Errorf("PVC deleted but cannot determine PV name (no annotation or status)")
			}
			pv = &corev1.PersistentVolume{}
			if err := r.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get PV %s: %w", pvName, err)
			}
		case apierrors.IsForbidden(err):
			return r.markFailed(ctx, vcr, storagev1alpha1.ConditionReasonRBACDenied, fmt.Sprintf("Access denied to PVC %s/%s", target.Namespace, target.Name))
		default:
			return ctrl.Result{}, fmt.Errorf("failed to get PVC: %w", err)
		}
	}

	if !pvcNotFound {
		// 3. Check if PVC is bound
		if pvc.Spec.VolumeName == "" {
			return r.markFailed(ctx, vcr, storagev1alpha1.ConditionReasonInternalError, fmt.Sprintf("PVC %s/%s is not bound", pvc.Namespace, pvc.Name))
		}

		// 4. Get PV and store its name in annotation for future reconciles
		pv = &corev1.PersistentVolume{}
		if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
			if apierrors.IsNotFound(err) {
				return r.markFailed(ctx, vcr, storagev1alpha1.ConditionReasonNotFound, fmt.Sprintf("PV %s not found", pvc.Spec.VolumeName))
			}
			return ctrl.Result{}, fmt.Errorf("failed to get PV: %w", err)
		}

		// Store PV name in annotation for future reconciles (when PVC is deleted)
		// Use Patch instead of Update to minimize conflicts and reduce churn
		if vcr.Annotations == nil || vcr.Annotations["storage-foundation.deckhouse.io/detach-pv-name"] != pv.Name {
			current := &storagev1alpha1.VolumeCaptureRequest{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get VCR for annotation patch: %w", err)
			}
			base := current.DeepCopy()
			if current.Annotations == nil {
				current.Annotations = make(map[string]string)
			}
			current.Annotations["storage-foundation.deckhouse.io/detach-pv-name"] = pv.Name
			if err := r.Patch(ctx, current, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to patch VCR annotation: %w", err)
			}
		}
	}

	// 5. Ensure PV is loaded (defensive check)
	if pv == nil {
		return ctrl.Result{}, fmt.Errorf("internal error: PV is nil (should not happen)")
	}

	// 6. Check if PV is already detached
	// NOTE: If PV.Spec.PersistentVolumeReclaimPolicy != Retain, detaching may lead to
	// PV deletion by kube-controller-manager. This is a known limitation and should be
	// documented for users. In production, PV should have ReclaimPolicy=Retain for Detach mode.
	// Note: We check both ClaimRef and annotation to ensure PV was detached by our controller
	if pv.Spec.ClaimRef == nil {
		// Check if PV has our detached annotation to confirm it was detached by VCR
		alreadyDetachedByVCR := false
		if pv.Annotations != nil {
			if val, ok := pv.Annotations["storage-foundation.deckhouse.io/detached"]; ok && val == "true" {
				alreadyDetachedByVCR = true
			}
		}
		if !alreadyDetachedByVCR {
			// PV was detached by someone else, not by VCR - this is unexpected
			l.Info("PV already detached but not by VCR", "pv", pv.Name)
		}
		// Already detached, update status
		setPersistentVolumeDataRef(vcr, target, pv.Name, string(pv.UID))
		if err := r.finalizeVCR(ctx, vcr, metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted, fmt.Sprintf("PV %s already detached", pv.Name)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 7. Delete PVC according to ADR (only if PVC still exists)
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
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
		}
	}

	// 8. Ensure ObjectKeeper exists FIRST - before detaching PV
	// INVARIANT: ObjectKeeper must exist before creating artifacts (PV)
	// This ensures proper ownership chain: ObjectKeeper → PV
	retainerName := NamePrefixRetainerPV + string(vcr.UID)
	objectKeeper, result, err := r.ensureObjectKeeper(ctx, retainerName, vcr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.RequeueAfter > 0 {
		return result, nil
	}

	// 9. Detach PV from PVC and set ownerRef in a single Patch operation
	// According to ADR, PV after Detach gets annotation storage-foundation.deckhouse.io/detached: "true"
	// NOTE: PV should have ReclaimPolicy=Retain to prevent accidental deletion
	// Re-read PV to get latest state before patching
	updatedPV := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pv.Name}, updatedPV); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to re-read PV before detach: %w", err)
	}
	base := updatedPV.DeepCopy()

	// Detach PV: remove ClaimRef and set annotation
	updatedPV.Spec.ClaimRef = nil
	if updatedPV.Annotations == nil {
		updatedPV.Annotations = make(map[string]string)
	}
	updatedPV.Annotations["storage-foundation.deckhouse.io/detached"] = "true"

	// Set ownerRef: ObjectKeeper → PV
	// INVARIANT: PV can have only one controller owner - ObjectKeeper.
	// If another controller needs to own PV, it must coordinate with ObjectKeeper.
	// Check if ObjectKeeper ownerRef already exists (with UID check to prevent conflicts)
	retainerOwnerRefExists := false
	for _, ref := range updatedPV.OwnerReferences {
		if ref.Kind == KindObjectKeeper && ref.Name == retainerName && ref.UID == objectKeeper.UID {
			retainerOwnerRefExists = true
			break
		}
	}
	if !retainerOwnerRefExists {
		controllerTrue := true
		blockOwnerDeletion := true
		updatedPV.OwnerReferences = append(updatedPV.OwnerReferences, metav1.OwnerReference{
			APIVersion:         APIGroupDeckhouse,
			Kind:               KindObjectKeeper,
			Name:               retainerName,
			UID:                objectKeeper.UID,
			Controller:         &controllerTrue,
			BlockOwnerDeletion: &blockOwnerDeletion,
		})
	}

	// Single Patch operation for both detach and ownerRef
	if err := r.Patch(ctx, updatedPV, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to detach PV and set ownerRef: %w", err)
	}
	l.Info("Detached PV from PVC and set ownerRef", "pv", updatedPV.Name)

	// 10. Update VCR status
	setPersistentVolumeDataRef(vcr, target, updatedPV.Name, string(updatedPV.UID))
	if err := r.finalizeVCR(ctx, vcr, metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted, fmt.Sprintf("PV %s detached", updatedPV.Name)); err != nil {
		return ctrl.Result{}, err
	}

	l.Info("VolumeCaptureRequest completed", "mode", "Detach", "pv", updatedPV.Name)
	return ctrl.Result{}, nil
}

// Helper functions

// ensureObjectKeeper ensures ObjectKeeper exists for the given VCR.
// ObjectKeeper is a pure ownership anchor that follows VCR lifecycle.
// It owns resulting artifacts (VSC/PV) and is automatically deleted when VCR is deleted.
//
// INVARIANT:
// - ObjectKeeper is created before any artifacts (VSC/PV)
// - ObjectKeeper follows VCR lifecycle (FollowObject mode)
// - ObjectKeeper has no TTL (TTL is managed by VCR controller)
//
// CONTRACT:
// - Creates ObjectKeeper if it doesn't exist
// - Validates that existing ObjectKeeper belongs to this VCR (UID check)
// - Returns error if validation fails
// - Enforces UID barrier (may return RequeueAfter if UID not yet populated)
// - Caller MUST respect returned ctrl.Result before proceeding
func (r *VolumeCaptureRequestController) ensureObjectKeeper(
	ctx context.Context,
	name string,
	vcr *storagev1alpha1.VolumeCaptureRequest,
) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	l := log.FromContext(ctx)
	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	err := r.Get(ctx, client.ObjectKey{Name: name}, objectKeeper)

	if apierrors.IsNotFound(err) {
		// Create ObjectKeeper
		objectKeeper = &deckhousev1alpha1.ObjectKeeper{
			TypeMeta: metav1.TypeMeta{
				APIVersion: APIGroupDeckhouse,
				Kind:       KindObjectKeeper,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
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
			return nil, ctrl.Result{}, fmt.Errorf("failed to create ObjectKeeper: %w", err)
		}
		l.Info("Created ObjectKeeper", "name", name)
		// Re-read ObjectKeeper to get UID populated by apiserver/fake client
		if err := r.Get(ctx, client.ObjectKey{Name: name}, objectKeeper); err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("failed to re-read ObjectKeeper after Create: %w", err)
		}
		// HARD BARRIER: UID must exist before creating artifacts with OwnerReference
		// If UID is still not populated (shouldn't happen with real apiserver, but possible with fake client), requeue
		if objectKeeper.UID == "" {
			l.Info("ObjectKeeper UID not assigned yet, requeue", "name", name)
			return nil, ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return objectKeeper, ctrl.Result{}, nil
	}

	if err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("failed to get ObjectKeeper: %w", err)
	}

	// ObjectKeeper exists - validate it belongs to this VCR
	// This protects against race conditions where VCR was deleted and recreated with same name
	if objectKeeper.Spec.FollowObjectRef == nil {
		return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has no FollowObjectRef", name)
	}
	// Validate UID (primary check)
	if objectKeeper.Spec.FollowObjectRef.UID != string(vcr.UID) {
		return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s belongs to another VCR (UID mismatch: expected %s, got %s)",
			name, string(vcr.UID), objectKeeper.Spec.FollowObjectRef.UID)
	}
	// Validate Mode (should be FollowObject)
	if objectKeeper.Spec.Mode != "FollowObject" {
		return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has invalid mode: expected FollowObject, got %s", name, objectKeeper.Spec.Mode)
	}
	// HARD BARRIER: UID must exist before creating artifacts with OwnerReference
	// If ObjectKeeper exists but UID is not populated (e.g., from fake client in tests), requeue
	if objectKeeper.UID == "" {
		l.Info("ObjectKeeper exists but UID not assigned yet, requeue", "name", name)
		return nil, ctrl.Result{RequeueAfter: time.Second}, nil
	}
	// Validate Kind, Namespace, and Name (additional cheap checks)
	if objectKeeper.Spec.FollowObjectRef.Kind != KindVolumeCaptureRequest {
		return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s FollowObjectRef.Kind mismatch: expected %s, got %s",
			name, KindVolumeCaptureRequest, objectKeeper.Spec.FollowObjectRef.Kind)
	}
	if objectKeeper.Spec.FollowObjectRef.Namespace != vcr.Namespace {
		return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s FollowObjectRef.Namespace mismatch: expected %s, got %s",
			name, vcr.Namespace, objectKeeper.Spec.FollowObjectRef.Namespace)
	}
	if objectKeeper.Spec.FollowObjectRef.Name != vcr.Name {
		return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s FollowObjectRef.Name mismatch: expected %s, got %s",
			name, vcr.Name, objectKeeper.Spec.FollowObjectRef.Name)
	}

	return objectKeeper, ctrl.Result{}, nil
}

func (r *VolumeCaptureRequestController) markFailed(ctx context.Context, vcr *storagev1alpha1.VolumeCaptureRequest, reason, message string) (ctrl.Result, error) {
	if err := r.finalizeVCR(ctx, vcr, metav1.ConditionFalse, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// finalizeVCR finalizes VCR by setting the Ready condition, CompletionTimestamp, and updating status.
// This is a unified helper to eliminate code duplication across all finalization paths. The
// completionTimestamp is what the garbage collector measures the retention TTL from.
func (r *VolumeCaptureRequestController) finalizeVCR(
	ctx context.Context,
	vcr *storagev1alpha1.VolumeCaptureRequest,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	now := metav1.Now()
	if vcr.Status.CompletionTimestamp == nil {
		vcr.Status.CompletionTimestamp = &now
	}
	setSingleCondition(&vcr.Status.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})

	// Update status (retry only for status conflicts)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeCaptureRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); err != nil {
			return err
		}
		base := current.DeepCopy()
		current.Status = vcr.Status
		return r.Status().Patch(ctx, current, client.MergeFrom(base))
	}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // Object deleted - fine
		}
		return fmt.Errorf("failed to update VCR status: %w", err)
	}

	return nil
}

func (r *VolumeCaptureRequestController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.VolumeCaptureRequest{}).
		Complete(r)
}

// cleanupVCRArtifacts deletes a VCR's produced artifact (VolumeSnapshotContent / PersistentVolume) ONLY
// when it is a true orphan — it carries no ownerReferences at all. This is the anti-leak for the case
// where the VCR produced an artifact but it never received its ObjectKeeper owner. Any artifact that has
// an owner (its VCR ObjectKeeper, or a bridging keeper such as a live DataImport's) is left to the
// ownerRef cascade / Kubernetes GC and is NOT touched here, so garbage-collecting the VCR never deletes an
// artifact another object is still retaining. Best-effort; used as the GC PreDelete hook.
func cleanupVCRArtifacts(ctx context.Context, cl client.Client, vcr *storagev1alpha1.VolumeCaptureRequest) error {
	if vcr.Status.Data == nil {
		return nil
	}
	artifact := vcr.Status.Data.ArtifactRef
	if artifact.Name == "" {
		return nil
	}
	switch artifact.Kind {
	case "VolumeSnapshotContent":
		content := &snapshotv1.VolumeSnapshotContent{}
		if err := cl.Get(ctx, client.ObjectKey{Name: artifact.Name}, content); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if len(content.OwnerReferences) > 0 {
			return nil
		}
		if err := cl.Delete(ctx, content); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	case "PersistentVolume":
		pv := &corev1.PersistentVolume{}
		if err := cl.Get(ctx, client.ObjectKey{Name: artifact.Name}, pv); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if len(pv.OwnerReferences) > 0 {
			return nil
		}
		if err := cl.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}
