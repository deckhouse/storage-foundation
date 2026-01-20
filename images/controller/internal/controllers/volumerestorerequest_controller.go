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
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"

	"github.com/deckhouse/storage-foundation/images/controller/pkg/config"
)

const (
	// restorePollInterval is the interval for polling target PVC status during restore
	// VRR controller polls PVC to check if external-provisioner has created and bound it
	restorePollInterval = 5 * time.Second
)

// VolumeRestoreRequestController reconciles VolumeRestoreRequest objects
//
// DESIGN NOTE:
//
// VolumeRestoreRequestController is a fire-and-forget service-request controller.
// It does NOT orchestrate restore operations - it only:
//  1. Validates that source exists (VSC or PV) and is ready
//  2. Creates ObjectKeeper (ownership anchor)
//  3. Observes result (target PVC created by external-provisioner and Bound)
//  4. Finalizes VRR status
//
// IMPORTANT ARCHITECTURAL DECISIONS (per ADR):
// - VolumeSnapshot objects MUST NOT be created for restore operations
// - external-provisioner (patched) handles restore directly:
//   - Watches VolumeRestoreRequest
//   - Calls CSI CreateVolume using VolumeSnapshotContent.spec.volumeSnapshotHandle (for VSC) or PV volumeHandle (for PV)
//   - Creates PersistentVolume and PersistentVolumeClaim without using VolumeSnapshot or dataSourceRef
//   - Binds PV to PVC
//   - MUST NOT update VRR status (VRR status is managed exclusively by VolumeRestoreRequestController)
//
// - snapshot-controller and external-snapshotter are NOT involved in restore flow
//
// VRR controller only observes and reports the result.
// VRR status is finalized exclusively by VolumeRestoreRequestController when it observes successful restore.
// This follows the same architectural pattern as VolumeCaptureRequestController.
//
// CRITICAL: Single-writer contract for VRR status
// ================================================
// external-provisioner MUST NOT update VRR status.
// VRR status is finalized exclusively by VolumeRestoreRequestController.
// This is a hard architectural constraint to prevent race conditions and ensure predictable behavior.
// If external-provisioner needs to report errors, it should use PVC/PV status or Kubernetes events,
// not VRR status. VRR controller will observe these and update VRR status accordingly.
type VolumeRestoreRequestController struct {
	client.Client
	APIReader client.Reader // Required: for reading StorageClass directly from API server (if needed in future)
	Scheme    *runtime.Scheme
	Config    *config.Options
}

func (r *VolumeRestoreRequestController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vrr", req.NamespacedName)

	var vrr storagev1alpha1.VolumeRestoreRequest
	if err := r.Get(ctx, req.NamespacedName, &vrr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		l.Error(err, "failed to get VolumeRestoreRequest")
		return ctrl.Result{}, err
	}

	// Skip if terminal (Ready=True or Ready=False)
	// NOTE: VRR is a fire-and-forget (one-time) operation. Once terminal, VRR will not be
	// reconciled again even if spec is changed. This is by design: VRR represents a single
	// restore operation. To create a new restore, create a new VRR.
	if isTerminal(vrr.Status.Conditions, storagev1alpha1.ConditionTypeReady) {
		// VRR is terminal - TTL cleanup is handled by background TTL scanner, not in reconcile loop
		// This ensures reconcile loop doesn't block on TTL checks
		l.V(1).Info("VRR is terminal, skipping reconcile")
		return ctrl.Result{}, nil
	}

	// Handle deletion
	// NOTE: No finalizer needed.
	// ObjectKeeper follows VRR lifecycle and owns all artifacts.
	// When VRR is deleted:
	//   - ObjectKeeper is automatically deleted (FollowObject)
	//   - GC deletes PVC through ownerRef
	if !vrr.DeletionTimestamp.IsZero() {
		l.Info("VRR is being deleted, skipping reconcile (ObjectKeeper will handle cleanup)")
		return ctrl.Result{}, nil
	}

	// Process based on source type
	switch vrr.Spec.SourceRef.Kind {
	case SourceKindVolumeSnapshotContent:
		return r.processVolumeSnapshotContentRestore(ctx, &vrr)
	case SourceKindPersistentVolume:
		return r.processPersistentVolumeRestore(ctx, &vrr)
	default:
		return r.markFailed(ctx, &vrr, storagev1alpha1.ConditionReasonInvalidSource, fmt.Sprintf("Unsupported source kind: %s", vrr.Spec.SourceRef.Kind))
	}
}

// ensureObjectKeeper creates or gets ObjectKeeper for VRR.
// ObjectKeeper follows VRR lifecycle and owns all created artifacts (PVC).
// This ensures proper cleanup when VRR is deleted.
func (r *VolumeRestoreRequestController) ensureObjectKeeper(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vrr", fmt.Sprintf("%s/%s", vrr.Namespace, vrr.Name))

	// Generate ObjectKeeper name based on VRR UID
	retainerName := NamePrefixRetainerPVC + string(vrr.UID)
	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	err := r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper)

	if apierrors.IsNotFound(err) {
		// Create ObjectKeeper
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
					Kind:       KindVolumeRestoreRequest,
					Namespace:  vrr.Namespace,
					Name:       vrr.Name,
					UID:        string(vrr.UID),
				},
			},
		}
		if err := r.Create(ctx, objectKeeper); err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("failed to create ObjectKeeper: %w", err)
		}
		l.Info("Created ObjectKeeper", "name", retainerName)
		// HARD BARRIER: UID must exist before creating PVC with OwnerReference
		// After Create(), controller-runtime populates objectKeeper.UID automatically,
		// but we cannot rely on it being populated immediately in the same reconcile.
		// Re-read ObjectKeeper to get UID populated by apiserver/fake client
		if err := r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper); err != nil {
			return nil, ctrl.Result{}, fmt.Errorf("failed to re-read ObjectKeeper after Create: %w", err)
		}
		// If UID is still not populated (shouldn't happen with real apiserver, but possible with fake client), requeue
		if objectKeeper.UID == "" {
			l.Info("ObjectKeeper UID not assigned yet, requeue", "name", retainerName)
			return nil, ctrl.Result{RequeueAfter: time.Second}, nil
		}
	} else if err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("failed to get ObjectKeeper: %w", err)
	} else {
		// ObjectKeeper exists - validate it belongs to this VRR
		// This protects against race conditions where VRR was deleted and recreated with same name
		if objectKeeper.Spec.FollowObjectRef == nil {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has no FollowObjectRef", retainerName)
		}
		// Validate UID (primary check)
		if objectKeeper.Spec.FollowObjectRef.UID != string(vrr.UID) {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s belongs to another VRR (UID mismatch: expected %s, got %s)",
				retainerName, string(vrr.UID), objectKeeper.Spec.FollowObjectRef.UID)
		}
		// Validate Mode (should be FollowObject)
		if objectKeeper.Spec.Mode != "FollowObject" {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s has invalid mode: expected FollowObject, got %s", retainerName, objectKeeper.Spec.Mode)
		}
		// HARD BARRIER: UID must exist before creating PVC with OwnerReference
		// If ObjectKeeper exists but UID is not populated (e.g., from fake client in tests), requeue
		if objectKeeper.UID == "" {
			l.Info("ObjectKeeper exists but UID not assigned yet, requeue", "name", retainerName)
			return nil, ctrl.Result{RequeueAfter: time.Second}, nil
		}
		// Validate Kind, Namespace, and Name (additional cheap checks)
		if objectKeeper.Spec.FollowObjectRef.Kind != KindVolumeRestoreRequest {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s FollowObjectRef.Kind mismatch: expected %s, got %s",
				retainerName, KindVolumeRestoreRequest, objectKeeper.Spec.FollowObjectRef.Kind)
		}
		if objectKeeper.Spec.FollowObjectRef.Namespace != vrr.Namespace {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s FollowObjectRef.Namespace mismatch: expected %s, got %s",
				retainerName, vrr.Namespace, objectKeeper.Spec.FollowObjectRef.Namespace)
		}
		if objectKeeper.Spec.FollowObjectRef.Name != vrr.Name {
			return nil, ctrl.Result{}, fmt.Errorf("ObjectKeeper %s FollowObjectRef.Name mismatch: expected %s, got %s",
				retainerName, vrr.Name, objectKeeper.Spec.FollowObjectRef.Name)
		}
	}

	return objectKeeper, ctrl.Result{}, nil
}

// processVolumeSnapshotContentRestore handles restore from VolumeSnapshotContent
//
// According to ADR, VRR is a service-request, not an orchestrator.
// VRR controller:
//  1. Validates that source VSC exists and is ReadyToUse
//  2. Creates ObjectKeeper (ownership anchor)
//  3. Observes result (target PVC created by external-provisioner and Bound)
//  4. Finalizes VRR status
//
// IMPORTANT: VolumeSnapshot objects MUST NOT be created.
// external-provisioner (patched) handles restore directly:
// - Watches VolumeRestoreRequest
// - Calls CSI CreateVolume using VSC.spec.volumeSnapshotHandle
// - Creates PV and PVC without VolumeSnapshot or dataSourceRef
// - Binds PV to PVC
// - MUST NOT update VRR status (VRR status is managed exclusively by VolumeRestoreRequestController)
//
// CRITICAL: external-provisioner MUST NOT update VRR status.
// VRR status is finalized exclusively by VolumeRestoreRequestController.
func (r *VolumeRestoreRequestController) processVolumeSnapshotContentRestore(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vrr", fmt.Sprintf("%s/%s", vrr.Namespace, vrr.Name), "source", "VolumeSnapshotContent")

	// 1. Validate source: Get and check CSI VolumeSnapshotContent exists and is ReadyToUse
	csiVSC := &snapshotv1.VolumeSnapshotContent{}
	if err := r.Get(ctx, client.ObjectKey{Name: vrr.Spec.SourceRef.Name}, csiVSC); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markFailed(ctx, vrr, storagev1alpha1.ConditionReasonNotFound, fmt.Sprintf("CSI VolumeSnapshotContent %s not found", vrr.Spec.SourceRef.Name))
		}
		return ctrl.Result{}, fmt.Errorf("failed to get CSI VolumeSnapshotContent: %w", err)
	}

	// Check if VSC is ReadyToUse (required for restore)
	if csiVSC.Status == nil || csiVSC.Status.ReadyToUse == nil || !*csiVSC.Status.ReadyToUse {
		// Check for terminal errors first
		if csiVSC.Status != nil && csiVSC.Status.Error != nil && csiVSC.Status.Error.Message != nil {
			errorMsg := *csiVSC.Status.Error.Message
			return r.markFailed(ctx, vrr, storagev1alpha1.ConditionReasonInternalError, fmt.Sprintf("CSI VolumeSnapshotContent has error: %s", errorMsg))
		}
		// VSC exists but not ready yet - requeue (not terminal error)
		l.Info("VSC not ReadyToUse yet, requeuing", "vsc", csiVSC.Name)
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	// 2. Create ObjectKeeper (idempotent)
	_, result, err := r.ensureObjectKeeper(ctx, vrr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.RequeueAfter > 0 {
		return result, nil
	}

	// 3. Observe result: Check if target PVC exists and is Bound
	// IMPORTANT: Target PVC MUST be created by external-provisioner.
	// VRR controller MUST NOT create PVC under any circumstances.
	// VRR controller only observes and finalizes status when PVC is Bound.
	targetPVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: vrr.Spec.TargetNamespace, Name: vrr.Spec.TargetPVCName}, targetPVC); err != nil {
		if apierrors.IsNotFound(err) {
			// Target PVC doesn't exist yet - external-provisioner hasn't created it
			// Requeue to wait for external-provisioner
			l.Info("Target PVC not found yet, waiting for external-provisioner", "namespace", vrr.Spec.TargetNamespace, "name", vrr.Spec.TargetPVCName)
			return ctrl.Result{RequeueAfter: restorePollInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to check target PVC: %w", err)
	}

	// 4. Check if PVC is Bound
	if targetPVC.Status.Phase != corev1.ClaimBound {
		// PVC exists but not Bound yet - external-provisioner is still working
		l.Info("Target PVC exists but not Bound yet, waiting for external-provisioner", "namespace", vrr.Spec.TargetNamespace, "name", vrr.Spec.TargetPVCName, "phase", targetPVC.Status.Phase)
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	// 5. Success: PVC is Bound - finalize VRR
	vrr.Status.TargetPVCRef = &storagev1alpha1.ObjectReference{
		Name:      vrr.Spec.TargetPVCName,
		Namespace: vrr.Spec.TargetNamespace,
	}
	if err := r.finalizeVRR(ctx, vrr, metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted, fmt.Sprintf("PVC %s/%s restored successfully", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName)); err != nil {
		return ctrl.Result{}, err
	}

	l.Info("VolumeRestoreRequest completed", "source", "VolumeSnapshotContent", "pvc", vrr.Spec.TargetPVCName)
	return ctrl.Result{}, nil
}

// processPersistentVolumeRestore handles restore from PersistentVolume
//
// According to ADR, VRR is a service-request, not an orchestrator.
// VRR controller:
//  1. Validates that source PV exists
//  2. Creates ObjectKeeper (ownership anchor)
//  3. Observes result (target PVC created by external-provisioner and Bound)
//  4. Finalizes VRR status
//
// IMPORTANT: VolumeSnapshot objects MUST NOT be created.
// external-provisioner (patched) handles restore directly:
// - Watches VolumeRestoreRequest
// - Calls CSI CreateVolume using PV.spec.csi.volumeHandle (or fallback copy)
// - Creates new PV and PVC without VolumeSnapshot or dataSourceRef
// - Binds PV to PVC
// - MUST NOT update VRR status (VRR status is managed exclusively by VolumeRestoreRequestController)
//
// CRITICAL: external-provisioner MUST NOT update VRR status.
// VRR status is finalized exclusively by VolumeRestoreRequestController.
func (r *VolumeRestoreRequestController) processPersistentVolumeRestore(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vrr", fmt.Sprintf("%s/%s", vrr.Namespace, vrr.Name), "source", "PersistentVolume")

	// 1. Validate source: Get and check PersistentVolume exists
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: vrr.Spec.SourceRef.Name}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markFailed(ctx, vrr, storagev1alpha1.ConditionReasonNotFound, fmt.Sprintf("PersistentVolume %s not found", vrr.Spec.SourceRef.Name))
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PersistentVolume: %w", err)
	}

	// 2. Create ObjectKeeper (idempotent)
	_, result, err := r.ensureObjectKeeper(ctx, vrr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if result.RequeueAfter > 0 {
		return result, nil
	}

	// 3. Observe result: Check if target PVC exists and is Bound
	// IMPORTANT: Target PVC MUST be created by external-provisioner.
	// VRR controller MUST NOT create PVC under any circumstances.
	// VRR controller only observes and finalizes status when PVC is Bound.
	targetPVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: vrr.Spec.TargetNamespace, Name: vrr.Spec.TargetPVCName}, targetPVC); err != nil {
		if apierrors.IsNotFound(err) {
			// Target PVC doesn't exist yet - external-provisioner hasn't created it
			// Requeue to wait for external-provisioner
			l.Info("Target PVC not found yet, waiting for external-provisioner", "namespace", vrr.Spec.TargetNamespace, "name", vrr.Spec.TargetPVCName)
			return ctrl.Result{RequeueAfter: restorePollInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to check target PVC: %w", err)
	}

	// 4. Check if PVC is Bound
	if targetPVC.Status.Phase != corev1.ClaimBound {
		// PVC exists but not Bound yet - external-provisioner is still working
		l.Info("Target PVC exists but not Bound yet, waiting for external-provisioner", "namespace", vrr.Spec.TargetNamespace, "name", vrr.Spec.TargetPVCName, "phase", targetPVC.Status.Phase)
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	// 5. Success: PVC is Bound - finalize VRR
	vrr.Status.TargetPVCRef = &storagev1alpha1.ObjectReference{
		Name:      vrr.Spec.TargetPVCName,
		Namespace: vrr.Spec.TargetNamespace,
	}
	if err := r.finalizeVRR(ctx, vrr, metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted, fmt.Sprintf("PVC %s/%s restored successfully from PV", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName)); err != nil {
		return ctrl.Result{}, err
	}

	l.Info("VolumeRestoreRequest completed", "source", "PersistentVolume", "pvc", vrr.Spec.TargetPVCName)
	return ctrl.Result{}, nil
}

// Helper functions

func (r *VolumeRestoreRequestController) markFailed(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
	reason, message string,
) (ctrl.Result, error) {
	if err := r.finalizeVRR(ctx, vrr, metav1.ConditionFalse, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// finalizeVRR finalizes VRR by setting Ready condition, CompletionTimestamp, updating status, and TTL annotation.
// This is a unified helper to eliminate code duplication across all finalization paths.
func (r *VolumeRestoreRequestController) finalizeVRR(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	now := metav1.Now()
	if vrr.Status.CompletionTimestamp == nil {
		vrr.Status.CompletionTimestamp = &now
	}
	setSingleCondition(&vrr.Status.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})

	// Update status (retry only for status conflicts)
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeRestoreRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vrr), current); err != nil {
			return err
		}
		base := current.DeepCopy()
		current.Status = vrr.Status
		return r.Status().Patch(ctx, current, client.MergeFrom(base))
	}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // Object deleted - fine
		}
		return fmt.Errorf("failed to update VRR status: %w", err)
	}

	// Update TTL annotation (informational only, best-effort, no retry)
	current := &storagev1alpha1.VolumeRestoreRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(vrr), current); err == nil {
		base := current.DeepCopy()
		r.setTTLAnnotation(current)
		_ = r.Patch(ctx, current, client.MergeFrom(base)) // Ignore errors - annotation is informational
	}

	return nil
}

// setTTLAnnotation sets TTL annotation on the object.
//
// IMPORTANT TTL SEMANTICS:
// - TTL annotation (storage.deckhouse.io/ttl) is INFORMATIONAL ONLY.
// - Actual TTL deletion timing is controlled by controller configuration (config.RequestTTL).
// - TTL scanner uses config.RequestTTL, NOT the annotation value.
// - Annotation is set for observability and post-mortem analysis, but does not affect deletion timing.
//
// TTL is set when Ready/Failed condition is set during finalization.
// TTL comes from configuration (storage-foundation module settings), not from VRR spec.
// If annotation already exists, it is not overwritten (idempotent).
func (r *VolumeRestoreRequestController) setTTLAnnotation(vrr *storagev1alpha1.VolumeRestoreRequest) {
	// Don't overwrite if annotation already exists
	if vrr.Annotations != nil {
		if _, exists := vrr.Annotations[AnnotationKeyTTL]; exists {
			return
		}
	}
	if vrr.Annotations == nil {
		vrr.Annotations = make(map[string]string)
	}
	// Get TTL from configuration (default: 10m)
	ttlStr := config.DefaultRequestTTLStr
	if r.Config != nil && r.Config.RequestTTLStr != "" {
		ttlStr = r.Config.RequestTTLStr
	}
	vrr.Annotations[AnnotationKeyTTL] = ttlStr
}

func (r *VolumeRestoreRequestController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.VolumeRestoreRequest{}).
		Complete(r)
}

// StartTTLScanner starts the TTL scanner.
// Should be called from manager.RunnableFunc to ensure it runs only on the leader replica.
// Scanner periodically lists all VRRs and deletes expired ones based on completionTimestamp + TTL.
//
// IMPORTANT: This method should be called from manager.RunnableFunc to ensure leader-only execution.
// RunnableFunc already runs in a separate goroutine, so we don't need an additional go statement.
// When leadership changes, ctx.Done() triggers graceful shutdown of the scanner.
// Scanner uses List() to get all VRRs and checks completionTimestamp + TTL from controller config.
// This approach is simpler than per-object reconcile and doesn't block the reconcile loop.
//
// TTL SCANNER CONTRACT:
//
// 1. Works only with terminal VRRs:
//   - Ready=True (completed successfully)
//   - Ready=False (failed, terminal error)
//   - Non-terminal VRRs are never touched
//
// 2. TTL source:
//   - TTL is ALWAYS taken from controller configuration (config.RequestTTL), NOT from VRR annotations
//   - TTL annotation (storage.deckhouse.io/ttl) is informational only and does not affect deletion timing
//   - This ensures predictable cluster-wide retention policy
//
// 3. TTL calculation:
//   - TTL starts counting from CompletionTimestamp (when VRR reaches Ready=True or Ready=False)
//   - Expiration time = CompletionTimestamp + config.RequestTTL
//   - Only VRRs with CompletionTimestamp are eligible for deletion
//
// 4. Scanner behavior:
//   - Scanner does NOT update status
//   - Scanner does NOT patch objects
//   - Scanner only performs List() and Delete() operations
//   - Deletion of VRR triggers cleanup through ObjectKeeper (FollowObject)
//
// 5. Leader-only execution:
//   - Scanner runs only on the leader replica (enforced by manager.RunnableFunc)
//   - When leadership changes, ctx.Done() triggers graceful shutdown
func (r *VolumeRestoreRequestController) StartTTLScanner(ctx context.Context, client client.Client) {
	// Scanner interval: check every 5 minutes
	// This is a reasonable balance between responsiveness and API load
	scannerInterval := 5 * time.Minute
	ticker := time.NewTicker(scannerInterval)
	defer ticker.Stop()

	l := log.FromContext(ctx)
	l.Info("TTL scanner started", "interval", scannerInterval)

	// Run immediately on startup, then periodically
	r.scanAndDeleteExpiredVRRs(ctx, client)

	for {
		select {
		case <-ctx.Done():
			l.Info("TTL scanner stopped (context cancelled)")
			return
		case <-ticker.C:
			r.scanAndDeleteExpiredVRRs(ctx, client)
		}
	}
}

// scanAndDeleteExpiredVRRs lists all VRRs and deletes those where completionTimestamp + TTL < now.
//
// IMPORTANT:
// TTL annotation (storage.deckhouse.io/ttl) is informational only.
// Actual TTL is controlled exclusively by controller configuration.
// This ensures predictable cluster-wide retention policy.
//
// TTL SEMANTICS:
// - TTL is ALWAYS taken from controller configuration (config.RequestTTL), NOT from VRR annotations.
// - TTL annotation (storage.deckhouse.io/ttl) is informational only and is IGNORED by the scanner.
// - This ensures consistent cleanup behavior: all VRRs use the same TTL policy defined by controller config.
// - TTL starts counting from CompletionTimestamp (when VRR reaches Ready=True or Ready=False).
func (r *VolumeRestoreRequestController) scanAndDeleteExpiredVRRs(ctx context.Context, client client.Client) {
	// Get TTL from controller config (this is the ONLY source of TTL timing)
	// TTL annotation is informational only and is ignored here
	defaultTTL := config.DefaultRequestTTL
	if r.Config != nil && r.Config.RequestTTL > 0 {
		defaultTTL = r.Config.RequestTTL
	}

	// Guard: if TTL is disabled (<= 0), skip scanning
	if defaultTTL <= 0 {
		log.FromContext(ctx).V(1).Info("TTL scanner disabled (ttl <= 0)")
		return
	}

	// List all VRRs across all namespaces
	vrrList := &storagev1alpha1.VolumeRestoreRequestList{}
	if err := client.List(ctx, vrrList); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list VolumeRestoreRequests for TTL scan")
		return
	}

	now := time.Now()
	deletedCount := 0
	skippedCount := 0

	for i := range vrrList.Items {
		vrr := &vrrList.Items[i]

		// Skip if not terminal (Ready=True or Ready=False)
		if !isTerminal(vrr.Status.Conditions, storagev1alpha1.ConditionTypeReady) {
			skippedCount++
			continue // Skip non-terminal VRRs
		}

		// Skip if no completionTimestamp
		if vrr.Status.CompletionTimestamp == nil {
			skippedCount++
			continue
		}

		// Check if TTL expired: completionTimestamp + defaultTTL < now
		completionTime := vrr.Status.CompletionTimestamp.Time
		expirationTime := completionTime.Add(defaultTTL)

		if now.After(expirationTime) {
			// TTL expired, delete the object
			log.FromContext(ctx).Info("TTL expired, deleting VolumeRestoreRequest",
				"namespace", vrr.Namespace,
				"name", vrr.Name,
				"completionTime", completionTime,
				"expirationTime", expirationTime,
				"ttl", defaultTTL)

			if err := client.Delete(ctx, vrr); err != nil {
				if apierrors.IsNotFound(err) {
					// Already deleted, that's fine (double-delete is safe)
					log.FromContext(ctx).V(1).Info("VRR already deleted during TTL scan",
						"namespace", vrr.Namespace,
						"name", vrr.Name)
				} else {
					log.FromContext(ctx).Error(err, "Failed to delete expired VolumeRestoreRequest",
						"namespace", vrr.Namespace,
						"name", vrr.Name)
				}
			} else {
				deletedCount++
			}
		}
	}

	if deletedCount > 0 || skippedCount > 0 {
		log.FromContext(ctx).V(1).Info("TTL scan completed",
			"total", len(vrrList.Items),
			"deleted", deletedCount,
			"skipped", skippedCount)
	}
}
