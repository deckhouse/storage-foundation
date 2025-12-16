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
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
//
// DESIGN NOTE:
//
// VolumeCaptureRequestController intentionally relies only on informer cache.
// Read-after-write consistency is NOT guaranteed and NOT required.
// Cluster configuration objects (e.g. VolumeSnapshotClass) must exist
// before creating VolumeCaptureRequest.
//
// Any misconfiguration is treated as terminal failure.
type VolumeCaptureRequestController struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.Options
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

	l = l.WithValues("generation", vcr.Generation, "observedGeneration", vcr.Status.ObservedGeneration)

	// Skip if already Ready
	// NOTE: VCR is a fire-and-forget (one-time) operation. Once Ready, VCR will not be
	// reconciled again even if spec is changed. This is by design: VCR represents a single
	// snapshot capture operation. To create a new snapshot, create a new VCR.
	//
	// IMPORTANT: Temporary VolumeSnapshot (proxy-VS) cleanup happens in step 12 of processSnapshotMode
	// before VCR status is set to Ready. This ensures VS is deleted immediately upon VCR completion.
	// If VCR is already Ready, VS should already be cleaned up (or never existed).
	if isConditionTrue(vcr.Status.Conditions, ConditionTypeReady) {
		// VCR is Ready - TTL cleanup is handled by background TTL scanner, not in reconcile loop
		// This ensures reconcile loop doesn't block on TTL checks
		l.V(1).Info("VCR is already Ready, skipping reconcile")
		return ctrl.Result{}, nil
	}

	// Skip if already Failed and observed
	// TTL cleanup is handled by background TTL scanner, not in reconcile loop
	if isConditionFalse(vcr.Status.Conditions, ConditionTypeReady) {
		if vcr.Status.ObservedGeneration == vcr.Generation {
			// VCR is Failed and observed - TTL cleanup is handled by background TTL scanner
			l.V(1).Info("VCR is already Failed and observed, skipping reconcile")
			return ctrl.Result{}, nil
		}
		l.Info("VCR is Failed but not observed, will retry", "observedGeneration", vcr.Status.ObservedGeneration, "generation", vcr.Generation)
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
	var err error
	l := log.FromContext(ctx).WithValues("vcr", fmt.Sprintf("%s/%s", vcr.Namespace, vcr.Name), "mode", "Snapshot")
	l.Info("Processing VolumeCaptureRequest in Snapshot mode")

	// 1. Validate spec
	if vcr.Spec.PersistentVolumeClaimRef == nil {
		l.Error(nil, "PersistentVolumeClaimRef is required")
		return r.markFailed(ctx, vcr, ErrorReasonInternalError, "PersistentVolumeClaimRef is required")
	}
	l = l.WithValues("pvc", fmt.Sprintf("%s/%s", vcr.Spec.PersistentVolumeClaimRef.Namespace, vcr.Spec.PersistentVolumeClaimRef.Name))

	// 2. Check RBAC: try to get PVC (similar to MCR)
	l.Info("Getting PVC", "pvc", fmt.Sprintf("%s/%s", vcr.Spec.PersistentVolumeClaimRef.Namespace, vcr.Spec.PersistentVolumeClaimRef.Name))
	pvc := &corev1.PersistentVolumeClaim{}
	if err = r.Get(ctx, client.ObjectKey{
		Namespace: vcr.Spec.PersistentVolumeClaimRef.Namespace,
		Name:      vcr.Spec.PersistentVolumeClaimRef.Name,
	}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			l.Error(err, "PVC not found")
			return r.markFailed(ctx, vcr, ErrorReasonNotFound, fmt.Sprintf("PVC %s/%s not found", vcr.Spec.PersistentVolumeClaimRef.Namespace, vcr.Spec.PersistentVolumeClaimRef.Name))
		}
		if apierrors.IsForbidden(err) {
			l.Error(err, "Access denied to PVC")
			return r.markFailed(ctx, vcr, ErrorReasonRBACDenied, fmt.Sprintf("Access denied to PVC %s/%s", vcr.Spec.PersistentVolumeClaimRef.Namespace, vcr.Spec.PersistentVolumeClaimRef.Name))
		}
		l.Error(err, "Failed to get PVC")
		return ctrl.Result{}, fmt.Errorf("failed to get PVC: %w", err)
	}
	l.Info("PVC found", "volumeName", pvc.Spec.VolumeName)

	// 3. Check if PVC is bound
	if pvc.Spec.VolumeName == "" {
		l.Error(nil, "PVC is not bound")
		return r.markFailed(ctx, vcr, ErrorReasonInternalError, fmt.Sprintf("PVC %s/%s is not bound", pvc.Namespace, pvc.Name))
	}

	// 4. Get PV to extract volume handle
	l.Info("Getting PV", "pv", pvc.Spec.VolumeName)
	pv := &corev1.PersistentVolume{}
	if err = r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			l.Error(err, "PV not found")
			return r.markFailed(ctx, vcr, ErrorReasonNotFound, fmt.Sprintf("PV %s not found", pvc.Spec.VolumeName))
		}
		l.Error(err, "Failed to get PV")
		return ctrl.Result{}, fmt.Errorf("failed to get PV: %w", err)
	}

	// Check if PV has CSI volume handle
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		l.Error(nil, "PV does not have CSI volume handle", "pv", pv.Name, "hasCSI", pv.Spec.CSI != nil)
		return r.markFailed(ctx, vcr, ErrorReasonInternalError, fmt.Sprintf("PV %s does not have CSI volume handle", pv.Name))
	}
	l.Info("PV found", "driver", pv.Spec.CSI.Driver, "volumeHandle", pv.Spec.CSI.VolumeHandle)

	// 5. Get VolumeSnapshotClass from StorageClass annotation (needed for VSC creation)
	// VCR uses VolumeSnapshotClass specified in StorageClass annotation storage.deckhouse.io/volumesnapshotclass
	// This is the Deckhouse way: StorageClass explicitly declares which VolumeSnapshotClass to use
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		l.Error(nil, "PVC does not have StorageClassName")
		return r.markFailed(ctx, vcr, ErrorReasonInternalError, "PVC does not have StorageClassName")
	}
	storageClassName := *pvc.Spec.StorageClassName
	l.Info("Getting StorageClass", "storageClass", storageClassName)
	storageClass := &storagev1.StorageClass{}
	if err = r.Get(ctx, client.ObjectKey{Name: storageClassName}, storageClass); err != nil {
		if apierrors.IsNotFound(err) {
			l.Error(err, "StorageClass not found")
			return r.markFailed(ctx, vcr, ErrorReasonNotFound, fmt.Sprintf("StorageClass %s not found", storageClassName))
		}
		if apierrors.IsForbidden(err) {
			l.Error(err, "Access denied to StorageClass")
			return r.markFailed(ctx, vcr, ErrorReasonRBACDenied, fmt.Sprintf("Access denied to StorageClass %s", storageClassName))
		}
		l.Error(err, "Failed to get StorageClass")
		return ctrl.Result{}, fmt.Errorf("failed to get StorageClass: %w", err)
	}
	// Get VolumeSnapshotClass name from StorageClass annotation
	vscClassName, ok := storageClass.Annotations["storage.deckhouse.io/volumesnapshotclass"]
	if !ok || vscClassName == "" {
		l.Error(nil, "StorageClass does not have storage.deckhouse.io/volumesnapshotclass annotation", "storageClass", storageClassName)
		return r.markFailed(ctx, vcr, ErrorReasonNotFound,
			fmt.Sprintf("StorageClass %s does not have storage.deckhouse.io/volumesnapshotclass annotation", storageClassName))
	}
	l.Info("Found VolumeSnapshotClass name from StorageClass annotation", "vscClass", vscClassName)
	// Get VolumeSnapshotClass
	vscClass := &snapshotv1.VolumeSnapshotClass{}
	if err = r.Get(ctx, client.ObjectKey{Name: vscClassName}, vscClass); err != nil {
		if apierrors.IsNotFound(err) {
			l.Error(err, "VolumeSnapshotClass not found")
			return r.markFailed(ctx, vcr, ErrorReasonNotFound, fmt.Sprintf("VolumeSnapshotClass %s (from StorageClass %s annotation) not found", vscClassName, storageClassName))
		}
		if apierrors.IsForbidden(err) {
			l.Error(err, "Access denied to VolumeSnapshotClass")
			return r.markFailed(ctx, vcr, ErrorReasonRBACDenied, fmt.Sprintf("Access denied to VolumeSnapshotClass %s (from StorageClass %s annotation)", vscClassName, storageClassName))
		}
		l.Error(err, "Failed to get VolumeSnapshotClass")
		return ctrl.Result{}, fmt.Errorf("failed to get VolumeSnapshotClass: %w", err)
	}
	// Validate that VolumeSnapshotClass driver matches PV CSI driver
	pvDriver := pv.Spec.CSI.Driver
	if vscClass.Driver != pvDriver {
		l.Error(nil, "VolumeSnapshotClass driver does not match PV CSI driver",
			"vscDriver", vscClass.Driver, "pvDriver", pvDriver)
		return r.markFailed(ctx, vcr, ErrorReasonInternalError,
			fmt.Sprintf("VolumeSnapshotClass %s driver %s does not match PV CSI driver %s", vscClassName, vscClass.Driver, pvDriver))
	}
	l.Info("Found VolumeSnapshotClass", "name", vscClass.Name, "driver", vscClass.Driver)

	// 6. Create ObjectKeeper FIRST - before creating VSC
	l.Info("Creating ObjectKeeper")
	// ObjectKeeper is a pure ownership anchor that follows VCR lifecycle.
	// It owns resulting artifacts (VSC/PV) and is automatically deleted when VCR is deleted.
	// TTL and request cleanup are handled by VCR controller, not ObjectKeeper.
	csiVSCName := fmt.Sprintf("snapshot-%s", string(vcr.UID))
	retainerName := NamePrefixRetainer + csiVSCName
	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	err = r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper)
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
		// After Create(), controller-runtime populates objectKeeper.UID automatically
		// We rely on informer cache - if UID is not populated, it will be available on next reconcile
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
	//
	// IMPORTANT: VCR does NOT recreate VSC after terminal errors.
	// If VSC exists but has errors, VCR will mark itself as Failed (see step 8).
	// This ensures VCR is a fire-and-forget operation without retry logic.
	//
	// Check if VSC already exists (idempotency - don't recreate)
	l.Info("Checking for existing VolumeSnapshotContent", "vscName", csiVSCName)
	csiVSC := &snapshotv1.VolumeSnapshotContent{}
	err = r.Get(ctx, client.ObjectKey{Name: csiVSCName}, csiVSC)
	if apierrors.IsNotFound(err) {
		l.Info("VolumeSnapshotContent not found, creating new one")
		// Create VSC with proper spec and ownerRefs
		volumeHandle := pv.Spec.CSI.VolumeHandle
		vscClassName := vscClass.Name // Use the found default VolumeSnapshotClass name
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
		if err = r.Create(ctx, csiVSC); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create VolumeSnapshotContent: %w", err)
		}
		l.Info("Created VolumeSnapshotContent", "name", csiVSCName)
		// After Create(), controller-runtime populates csiVSC.UID automatically
		// We rely on informer cache - if UID is not populated, it will be available on next reconcile
		// Newly created VSC won't have Status yet - requeue to let external-snapshotter process it
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get VolumeSnapshotContent: %w", err)
	}
	// VSC exists - continue to wait for ReadyToUse (idempotency)

	// 8. Wait for external-snapshotter to set ReadyToUse=true on VSC
	//
	// Decision algorithm (according to ADR):
	//   if VCR Ready or Failed (observed):
	//       exit reconcile
	//   if VSC not found:
	//       create VSC
	//       requeue
	//   if VSC.status.error exists (regardless of ReadyToUse):
	//       mark VCR Failed
	//       exit reconcile
	//   if VSC.status.readyToUse == true:
	//       mark VCR Ready
	//       exit reconcile
	//   else:
	//       requeue (wait for snapshot-controller)
	//
	// VCR is a fire-and-forget operation:
	//   - VCR does NOT recreate VSC after terminal errors
	//   - VCR does NOT try to "heal" VSC
	//   - VSC.status.error is the single source of truth for snapshot errors
	//   - Any terminal error in VSC → terminal state in VCR
	//   - Error has priority over ReadyToUse (if both exist, error wins)

	// Check for terminal CSI errors FIRST (error has priority over ReadyToUse)
	// Terminal error: Status.Error.Message is set and non-empty
	// Examples: "provided secret is empty", NotFound/Forbidden for secrets, invalid arguments, PermissionDenied
	// VCR does NOT interpret CSI error codes - it only checks for presence of error message
	// IMPORTANT: Check error even if ReadyToUse==true (error has priority)
	if csiVSC.Status != nil && csiVSC.Status.Error != nil && csiVSC.Status.Error.Message != nil && *csiVSC.Status.Error.Message != "" {
		errorMsg := *csiVSC.Status.Error.Message
		// Include VSC name and PVC name in error message for better debugging
		errorDetails := fmt.Sprintf("VSC %s, PVC %s/%s: %s", csiVSCName, pvc.Namespace, pvc.Name, errorMsg)
		// Include error time if available
		if csiVSC.Status.Error.Time != nil {
			errorDetails = fmt.Sprintf("%s (error time: %s)", errorDetails, csiVSC.Status.Error.Time.Format(time.RFC3339))
		}
		// Mark VCR as failed with DataRef pointing to the problematic VSC
		// This is a terminal error - VCR will not retry or recreate VSC
		return r.markFailedSnapshot(ctx, vcr, csiVSCName, ErrorReasonSnapshotCreationFailed,
			fmt.Sprintf("CSI snapshot creation failed: %s", errorDetails))
	}

	// Check ReadyToUse status (only if no error)
	if csiVSC.Status == nil || csiVSC.Status.ReadyToUse == nil || !*csiVSC.Status.ReadyToUse {
		// Temporary state: VSC exists but snapshot-controller hasn't processed it yet
		// This is NOT an error - just wait for snapshot-controller to update status
		l.Info("Waiting for external-snapshotter to set ReadyToUse=true", "name", csiVSCName)
		// Use longer requeue interval for snapshot operations to avoid API pressure
		// External-snapshotter needs time to call CSI CreateSnapshot and update status
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// 9. Update VCR status (VSC is ready)
	// VSC.status.readyToUse == true → mark VCR as Ready (terminal success state)
	// VCR is fire-and-forget: once completed, we observe the current generation and never reconcile again
	now := metav1.Now()
	// Use retry on conflict to handle concurrent updates
	// Use ObjectKeyFromObject to ensure we have correct namespace/name
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeCaptureRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); err != nil {
			return err
		}
		// Create base for status patch BEFORE mutating status
		baseStatus := current.DeepCopy()
		// Update status
		current.Status.DataRef = &storagev1alpha1.ObjectReference{
			Name: csiVSCName,
			Kind: "VolumeSnapshotContent", // Explicitly set Kind according to ADR
		}
		// VCR is fire-and-forget: once completed, we observe the current generation and never reconcile again
		current.Status.ObservedGeneration = current.Generation
		current.Status.CompletionTimestamp = &now
		setSingleCondition(&current.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             ConditionReasonCompleted,
			Message:            fmt.Sprintf("CSI VolumeSnapshotContent %s ready", csiVSCName),
			LastTransitionTime: now,
			ObservedGeneration: current.Generation,
		})
		// Patch status using Status().Patch() to ensure proper subresource handling
		if err := r.Status().Patch(ctx, current, client.MergeFrom(baseStatus)); err != nil {
			return err
		}
		// After status patch, create new base for metadata patch (object state has changed)
		baseMeta := current.DeepCopy()
		// Set TTL annotation
		r.setTTLAnnotation(current)
		// Patch metadata (annotations) separately using fresh base
		return r.Patch(ctx, current, client.MergeFrom(baseMeta))
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

// setTTLAnnotation sets TTL annotation on the object.
// TTL annotation is informational only and does not affect deletion timing.
// Actual TTL is controlled exclusively by controller configuration (config.RequestTTL).
// This ensures predictable cluster-wide retention policy.
//
// IMPORTANT:
// TTL annotation (storage.deckhouse.io/ttl) is informational only.
// Actual TTL is controlled exclusively by controller configuration.
// This ensures predictable cluster-wide retention policy.
//
// The annotation is set when Ready/Failed condition is set for operator visibility.
// If annotation already exists, it is not overwritten (idempotent).
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
	// This is informational only - actual deletion uses config.RequestTTL
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
	return r.markFailedSnapshot(ctx, vcr, "", reason, message)
}

// markFailedSnapshot marks VCR as failed with optional DataRef pointing to VSC
//
// This is used when snapshot creation fails and we want to reference the problematic VSC.
// After calling this function, VCR enters a terminal Failed state and will not be reconciled again.
//
// VCR invariants:
//   - Any terminal error in VSC → terminal state in VCR
//   - VCR does NOT try to "heal" VSC or recreate it
//   - VSC.status.error is the single source of truth for snapshot errors
func (r *VolumeCaptureRequestController) markFailedSnapshot(ctx context.Context, vcr *storagev1alpha1.VolumeCaptureRequest, vscName, reason, message string) (ctrl.Result, error) {
	vcrBase := vcr.DeepCopy()
	vcr.Status.ObservedGeneration = vcr.Generation
	now := metav1.Now()
	vcr.Status.CompletionTimestamp = &now

	// Set DataRef to VSC if provided (for snapshot failures)
	if vscName != "" {
		vcr.Status.DataRef = &storagev1alpha1.ObjectReference{
			Name: vscName,
			Kind: "VolumeSnapshotContent",
		}
	}

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

// StartTTLScanner starts the TTL scanner in a background goroutine.
// Should be called from manager.RunnableFunc to ensure it runs only on the leader replica.
// Scanner periodically lists all VCRs and deletes expired ones based on completionTimestamp + TTL.
//
// IMPORTANT: This method should be called from manager.RunnableFunc to ensure leader-only execution.
// When leadership changes, ctx.Done() triggers graceful shutdown of the scanner.
func (r *VolumeCaptureRequestController) StartTTLScanner(ctx context.Context, client client.Client) {
	go r.startTTLScanner(ctx, client)
}

// startTTLScanner runs a background scanner that periodically checks and deletes expired VCRs.
// Scanner uses List() to get all VCRs and checks completionTimestamp + TTL from controller config.
// This approach is simpler than per-object reconcile and doesn't block the reconcile loop.
//
// TTL SCANNER CONTRACT:
//
// 1. Works only with terminal VCRs:
//   - Ready=True (completed successfully)
//   - Ready=False (failed)
//   - Non-terminal VCRs are never touched
//
// 2. TTL source:
//   - TTL is ALWAYS taken from controller configuration (config.RequestTTL), NOT from VCR annotations
//   - TTL annotation (storage.deckhouse.io/ttl) is informational only and does not affect deletion timing
//   - This ensures predictable cluster-wide retention policy
//
// 3. TTL calculation:
//   - TTL starts counting from CompletionTimestamp (when VCR reaches Ready=True or Failed=True)
//   - Expiration time = CompletionTimestamp + config.RequestTTL
//   - Only VCRs with CompletionTimestamp are eligible for deletion
//
// 4. Scanner behavior:
//   - Scanner does NOT update status
//   - Scanner does NOT patch objects
//   - Scanner only performs List() and Delete() operations
//   - Deletion of VCR triggers GC of ObjectKeeper and VSC through ownerReferences
//
// 5. Leader-only execution:
//   - Scanner runs only on the leader replica (enforced by manager.RunnableFunc)
//   - When leadership changes, ctx.Done() triggers graceful shutdown
func (r *VolumeCaptureRequestController) startTTLScanner(ctx context.Context, client client.Client) {
	// Scanner interval: check every 5 minutes
	// This is a reasonable balance between responsiveness and API load
	scannerInterval := 5 * time.Minute
	ticker := time.NewTicker(scannerInterval)
	defer ticker.Stop()

	l := log.FromContext(ctx)
	l.Info("TTL scanner started", "interval", scannerInterval)

	// Run immediately on startup, then periodically
	r.scanAndDeleteExpiredVCRs(ctx, client)

	for {
		select {
		case <-ctx.Done():
			l.Info("TTL scanner stopped (context cancelled)")
			return
		case <-ticker.C:
			r.scanAndDeleteExpiredVCRs(ctx, client)
		}
	}
}

// scanAndDeleteExpiredVCRs lists all VCRs and deletes those where completionTimestamp + TTL < now.
//
// IMPORTANT:
// TTL annotation (storage.deckhouse.io/ttl) is informational only.
// Actual TTL is controlled exclusively by controller configuration.
// This ensures predictable cluster-wide retention policy.
//
// TTL SEMANTICS:
// - TTL is ALWAYS taken from controller configuration (config.RequestTTL), NOT from VCR annotations.
// - TTL annotation (storage.deckhouse.io/ttl) is informational only and is IGNORED by the scanner.
// - This ensures consistent cleanup behavior: all VCRs use the same TTL policy defined by controller config.
// - TTL starts counting from CompletionTimestamp (when VCR reaches Ready=True or Failed=True).
func (r *VolumeCaptureRequestController) scanAndDeleteExpiredVCRs(ctx context.Context, client client.Client) {
	// Get TTL from controller config (this is the ONLY source of TTL timing)
	// TTL annotation is informational only and is ignored here
	defaultTTL := config.DefaultRequestTTL
	if r.Config != nil && r.Config.RequestTTL > 0 {
		defaultTTL = r.Config.RequestTTL
	}

	// List all VCRs across all namespaces
	vcrList := &storagev1alpha1.VolumeCaptureRequestList{}
	if err := client.List(ctx, vcrList); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list VolumeCaptureRequests for TTL scan")
		return
	}

	now := time.Now()
	deletedCount := 0
	skippedCount := 0

	for i := range vcrList.Items {
		vcr := &vcrList.Items[i]

		// Skip if not terminal (Ready=True or Failed=True)
		readyCondition := meta.FindStatusCondition(vcr.Status.Conditions, ConditionTypeReady)
		isTerminal := (readyCondition != nil && readyCondition.Status == metav1.ConditionTrue) ||
			(readyCondition != nil && readyCondition.Status == metav1.ConditionFalse)

		if !isTerminal {
			skippedCount++
			continue // Skip non-terminal VCRs
		}

		// Skip if no completionTimestamp
		if vcr.Status.CompletionTimestamp == nil {
			skippedCount++
			continue
		}

		// Check if TTL expired: completionTimestamp + defaultTTL < now
		completionTime := vcr.Status.CompletionTimestamp.Time
		expirationTime := completionTime.Add(defaultTTL)

		if now.After(expirationTime) {
			// TTL expired, delete the object
			log.FromContext(ctx).Info("TTL expired, deleting VolumeCaptureRequest",
				"namespace", vcr.Namespace,
				"name", vcr.Name,
				"completionTime", completionTime,
				"expirationTime", expirationTime,
				"ttl", defaultTTL)

			if err := client.Delete(ctx, vcr); err != nil {
				if apierrors.IsNotFound(err) {
					// Already deleted, that's fine (double-delete is safe)
					log.FromContext(ctx).V(1).Info("VCR already deleted during TTL scan",
						"namespace", vcr.Namespace,
						"name", vcr.Name)
				} else {
					log.FromContext(ctx).Error(err, "Failed to delete expired VolumeCaptureRequest",
						"namespace", vcr.Namespace,
						"name", vcr.Name)
				}
			} else {
				deletedCount++
			}
		}
	}

	if deletedCount > 0 || skippedCount > 0 {
		log.FromContext(ctx).V(1).Info("TTL scan completed",
			"total", len(vcrList.Items),
			"deleted", deletedCount,
			"skipped", skippedCount)
	}
}
