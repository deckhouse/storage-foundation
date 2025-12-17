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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "fox.flant.com/deckhouse/storage/storage-foundation/api/v1alpha1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"

	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/config"
	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/snapshotmeta"
)

const (
	// DefaultRequeueInterval is the default interval for requeue
	DefaultRequeueInterval = 10 * time.Second
)

// VolumeRestoreRequestController reconciles VolumeRestoreRequest objects
//
// StorageClass is read via APIReader (direct API, no cache) since it's cluster-level
// configuration that doesn't need to be watched or cached.
//
// Controllers MUST read StorageClass via APIReader. APIReader is a required dependency.
type VolumeRestoreRequestController struct {
	client.Client
	APIReader client.Reader // Required: for reading StorageClass directly from API server
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

	// Skip if already Ready
	if isConditionTrue(vrr.Status.Conditions, storagev1alpha1.ConditionTypeReady) {
		// Check TTL for completed VRR (after Ready short-circuit)
		// This ensures TTL cleanup happens only after VRR is fully completed
		if shouldDelete, requeueAfter, err := r.checkAndHandleTTL(ctx, &vrr); err != nil {
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
	if isConditionFalse(vrr.Status.Conditions, storagev1alpha1.ConditionTypeReady) {
		if vrr.Status.ObservedGeneration == vrr.Generation {
			// Check TTL for failed VRR (after Failed short-circuit)
			// This ensures TTL cleanup happens only after VRR is fully failed
			if shouldDelete, requeueAfter, err := r.checkAndHandleTTL(ctx, &vrr); err != nil {
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
	if !vrr.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &vrr)
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

// processVolumeSnapshotContentRestore restores PVC from VolumeSnapshotContent
// According to ADR, VRR is the unified path for all restore operations.
// external-provisioner creates PV+PVC through CreateVolume(snapshot/clone).
func (r *VolumeRestoreRequestController) processVolumeSnapshotContentRestore(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vrr", fmt.Sprintf("%s/%s", vrr.Namespace, vrr.Name), "source", "VolumeSnapshotContent")
	// 1. Get CSI VolumeSnapshotContent
	csiVSC := &snapshotv1.VolumeSnapshotContent{}
	if err := r.Get(ctx, client.ObjectKey{Name: vrr.Spec.SourceRef.Name}, csiVSC); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markFailed(ctx, vrr, storagev1alpha1.ConditionReasonNotFound, fmt.Sprintf("CSI VolumeSnapshotContent %s not found", vrr.Spec.SourceRef.Name))
		}
		return ctrl.Result{}, fmt.Errorf("failed to get CSI VolumeSnapshotContent: %w", err)
	}

	// 2. Ensure service namespace
	serviceNS := vrr.Spec.ServiceNamespace
	if serviceNS == "" {
		serviceNS = ServiceNamespace
	}
	if err := r.ensureNamespace(ctx, serviceNS); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to ensure service namespace: %w", err)
	}

	// 3. Check if target PVC already exists
	// According to ADR, VRR errors if PVC with such name exists (no magic, predictable behavior)
	// However, we need to handle idempotent reconcile: if PVC was created by us but status not updated yet
	targetPVC := &corev1.PersistentVolumeClaim{}
	err2 := r.Get(ctx, client.ObjectKey{Namespace: vrr.Spec.TargetNamespace, Name: vrr.Spec.TargetPVCName}, targetPVC)
	if err2 == nil {
		// Target PVC already exists
		// If it's Bound, consider this a successful restore and update status
		if targetPVC.Status.Phase == corev1.ClaimBound {
			// Find and cleanup temporary VolumeSnapshot if exists
			csiVSList := &snapshotv1.VolumeSnapshotList{}
			if err := r.List(ctx, csiVSList, client.InNamespace(serviceNS), client.MatchingLabels{
				LabelKeyCreatedBy: LabelValueCreatedByVRR,
			}); err == nil {
				for i := range csiVSList.Items {
					vs := &csiVSList.Items[i]
					for _, ownerRef := range vs.OwnerReferences {
						if ownerRef.Kind == "VolumeRestoreRequest" && ownerRef.Name == vrr.Name && ownerRef.UID == vrr.UID {
							if err := r.Delete(ctx, vs); err != nil && !apierrors.IsNotFound(err) {
								l.Error(err, "Failed to delete temporary VolumeSnapshot", "name", vs.Name)
							}
							break
						}
					}
				}
			}
			// Update status to Ready
			base := vrr.DeepCopy()
			vrr.Status.TargetPVCRef = &storagev1alpha1.ObjectReference{
				Name:      vrr.Spec.TargetPVCName,
				Namespace: vrr.Spec.TargetNamespace,
			}
			vrr.Status.ObservedGeneration = vrr.Generation
			now := metav1.Now()
			vrr.Status.CompletionTimestamp = &now
			setSingleCondition(&vrr.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             storagev1alpha1.ConditionReasonCompleted,
				Message:            fmt.Sprintf("PVC %s/%s restored successfully", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName),
				LastTransitionTime: now,
				ObservedGeneration: vrr.Generation,
			})
			// Set TTL annotation when marking as Ready (same Patch as Ready condition)
			// Use retry on conflict to handle concurrent updates
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				current := &storagev1alpha1.VolumeRestoreRequest{}
				if err := r.Get(ctx, client.ObjectKeyFromObject(vrr), current); err != nil {
					return err
				}
				// Apply status changes
				current.Status = vrr.Status
				// Set TTL annotation
				r.setTTLAnnotation(current)
				// Patch both metadata (annotations) and status in the same operation
				return r.Patch(ctx, current, client.MergeFrom(base))
			}); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update VRR with Ready condition and TTL annotation: %w", err)
			}
			l.Info("VolumeRestoreRequest completed (PVC already Bound)", "source", "VolumeSnapshotContent", "pvc", vrr.Spec.TargetPVCName)
			return ctrl.Result{}, nil
		}
		// PVC exists but not Bound yet - requeue instead of failing
		// This handles the case where PVC was created by us but status not updated yet
		l.Info("Target PVC exists but not Bound yet, requeuing", "namespace", vrr.Spec.TargetNamespace, "name", vrr.Spec.TargetPVCName, "phase", targetPVC.Status.Phase)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}
	if !apierrors.IsNotFound(err2) {
		return ctrl.Result{}, fmt.Errorf("failed to check target PVC: %w", err2)
	}

	// 4. Ensure VolumeSnapshotRef.Namespace is set on VSC (needed by snapshot-controller)
	// VSC might have been created without Namespace (e.g., created outside of Capture flow)
	csiVSCBase := csiVSC.DeepCopy()
	if csiVSC.Spec.VolumeSnapshotRef.Namespace == "" {
		// Use serviceNS as the namespace where VS will be created
		csiVSC.Spec.VolumeSnapshotRef.Namespace = serviceNS
		if err := r.Patch(ctx, csiVSC, client.MergeFrom(csiVSCBase)); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set VolumeSnapshotRef.Namespace on VSC: %w", err)
		}
		l.Info("Set VolumeSnapshotRef.Namespace on VSC", "name", csiVSC.Name, "namespace", serviceNS)
		// Re-read VSC to get updated version
		if err := r.Get(ctx, client.ObjectKey{Name: csiVSC.Name}, csiVSC); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to re-read VSC after setting Namespace: %w", err)
		}
	}

	// 5. Create or get temporary CSI VolumeSnapshot in service namespace
	// Note: According to ADR, VolumeSnapshot is only a temporary compatibility object
	csiVSName, err4 := r.ensureTemporaryVolumeSnapshot(ctx, serviceNS, csiVSC, vrr)
	if err4 != nil {
		return ctrl.Result{}, fmt.Errorf("failed to ensure temporary VolumeSnapshot: %w", err4)
	}

	// 6. Wait for CSI VolumeSnapshot to be ReadyToUse
	csiVS := &snapshotv1.VolumeSnapshot{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: serviceNS, Name: csiVSName}, csiVS); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get CSI VolumeSnapshot: %w", err)
	}
	if csiVS.Status == nil || csiVS.Status.ReadyToUse == nil || !*csiVS.Status.ReadyToUse {
		l.Info("CSI VolumeSnapshot not ready yet", "name", csiVSName)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}

	// 7. Validate StorageClass compatibility
	// According to ADR, restore is prohibited if SC=WFFC (WaitForFirstConsumer)
	// According to ADR, cross-SC restore is not supported (VRR gets Incompatible condition)
	if vrr.Spec.StorageClassName != "" {
		targetSC := &storagev1.StorageClass{}
		if err := r.Get(ctx, client.ObjectKey{Name: vrr.Spec.StorageClassName}, targetSC); err != nil {
			if apierrors.IsNotFound(err) {
				return r.markFailed(ctx, vrr, storagev1alpha1.ConditionReasonNotFound, fmt.Sprintf("StorageClass %s not found", vrr.Spec.StorageClassName))
			}
			return ctrl.Result{}, fmt.Errorf("failed to get StorageClass: %w", err)
		}

		// Check for WFFC (WaitForFirstConsumer)
		if targetSC.VolumeBindingMode != nil && *targetSC.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer {
			return r.markIncompatible(ctx, vrr, "StorageClass uses WaitForFirstConsumer volume binding mode, restore is not supported")
		}

		// Check for cross-SC restore: get source StorageClass from VSC
		// VSC should have annotation or label with source StorageClass, or we can get it from original PVC
		// For now, we check if VSC has source PVC info in labels
		if sourcePVCNS, ok := csiVSC.Labels[LabelKeySourcePVCNamespace]; ok {
			if sourcePVCName, ok := csiVSC.Labels[LabelKeySourcePVCName]; ok {
				sourcePVC := &corev1.PersistentVolumeClaim{}
				if err := r.Get(ctx, client.ObjectKey{Namespace: sourcePVCNS, Name: sourcePVCName}, sourcePVC); err == nil {
					if sourcePVC.Spec.StorageClassName != nil && *sourcePVC.Spec.StorageClassName != "" {
						if *sourcePVC.Spec.StorageClassName != vrr.Spec.StorageClassName {
							return r.markIncompatible(ctx, vrr, fmt.Sprintf("Cross-StorageClass restore not supported: source SC=%s, target SC=%s", *sourcePVC.Spec.StorageClassName, vrr.Spec.StorageClassName))
						}
					}
				}
			}
		}
	}

	// 8. Create target PVC directly in target namespace (using CSI VolumeSnapshot from service namespace)
	// According to ADR, external-provisioner creates PV+PVC through CreateVolume(snapshot/clone)
	// Note: PVC can reference VolumeSnapshot from another namespace via dataSource
	targetPVC = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vrr.Spec.TargetPVCName,
			Namespace: vrr.Spec.TargetNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: stringPtrOrNil(vrr.Spec.StorageClassName),
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: r.getSizeFromCSIVSC(csiVSC),
				},
			},
			// According to ADR, use DataSourceRef only (not DataSource)
			// DataSource and DataSourceRef together cause undefined behavior
			DataSourceRef: &corev1.TypedObjectReference{
				APIGroup:  func() *string { s := APIGroupSnapshotStorage; return &s }(),
				Kind:      KindVolumeSnapshot,
				Name:      csiVSName,
				Namespace: &serviceNS,
			},
		},
	}

	if err := r.Create(ctx, targetPVC); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// PVC was created by another reconcile, requeue
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to create target PVC: %w", err)
	}
	l.Info("Created target PVC", "name", vrr.Spec.TargetPVCName, "namespace", vrr.Spec.TargetNamespace)

	// 8. Check if target PVC is Bound (requeue if not)
	bound, requeue, err := r.checkPVCBound(ctx, vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check target PVC: %w", err)
	}
	if !bound {
		return requeue, nil
	}

	// 9. Cleanup temporary objects in service namespace
	if err := r.cleanupServiceNamespace(ctx, serviceNS, csiVSName); err != nil {
		l.Error(err, "Failed to cleanup service namespace", "namespace", serviceNS)
		// Don't fail the restore if cleanup fails
	}

	// 10. Update status
	base := vrr.DeepCopy()
	vrr.Status.TargetPVCRef = &storagev1alpha1.ObjectReference{
		Name:      vrr.Spec.TargetPVCName,
		Namespace: vrr.Spec.TargetNamespace,
	}
	vrr.Status.ObservedGeneration = vrr.Generation
	now := metav1.Now()
	vrr.Status.CompletionTimestamp = &now
	setSingleCondition(&vrr.Status.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             storagev1alpha1.ConditionReasonCompleted,
		Message:            fmt.Sprintf("PVC %s/%s restored successfully", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName),
		LastTransitionTime: now,
		ObservedGeneration: vrr.Generation,
	})
	// Set TTL annotation when marking as Ready (same Patch as Ready condition)
	// Use retry on conflict to handle concurrent updates
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeRestoreRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vrr), current); err != nil {
			return err
		}
		// Apply status changes
		current.Status = vrr.Status
		// Set TTL annotation
		r.setTTLAnnotation(current)
		// Patch both metadata (annotations) and status in the same operation
		return r.Patch(ctx, current, client.MergeFrom(base))
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update VRR with Ready condition and TTL annotation: %w", err)
	}

	l.Info("VolumeRestoreRequest completed", "source", "VolumeSnapshotContent", "pvc", vrr.Spec.TargetPVCName)
	return ctrl.Result{}, nil
}

// processPersistentVolumeRestore restores PVC from PersistentVolume
// According to ADR, VRR is the unified path for all restore operations.
// external-provisioner creates PV+PVC through CreateVolume(clone/copy fallback for existing PV).
func (r *VolumeRestoreRequestController) processPersistentVolumeRestore(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vrr", fmt.Sprintf("%s/%s", vrr.Namespace, vrr.Name), "source", "PersistentVolume")
	// 1. Get PersistentVolume
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: vrr.Spec.SourceRef.Name}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markFailed(ctx, vrr, storagev1alpha1.ConditionReasonNotFound, fmt.Sprintf("PersistentVolume %s not found", vrr.Spec.SourceRef.Name))
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PersistentVolume: %w", err)
	}

	// 2. Ensure service namespace
	serviceNS := vrr.Spec.ServiceNamespace
	if serviceNS == "" {
		serviceNS = ServiceNamespace
	}
	if err := r.ensureNamespace(ctx, serviceNS); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to ensure service namespace: %w", err)
	}

	// 3. Check if target PVC already exists
	// According to ADR, VRR errors if PVC with such name exists (no magic, predictable behavior)
	// However, we need to handle idempotent reconcile: if PVC was created by us but status not updated yet
	targetPVC := &corev1.PersistentVolumeClaim{}
	err2 := r.Get(ctx, client.ObjectKey{Namespace: vrr.Spec.TargetNamespace, Name: vrr.Spec.TargetPVCName}, targetPVC)
	if err2 == nil {
		// Target PVC already exists
		// If it's Bound, consider this a successful restore and update status
		if targetPVC.Status.Phase == corev1.ClaimBound {
			// Find and cleanup temporary PVC if exists
			tempPVCNameBase := fmt.Sprintf("%s%s-%s", NamePrefixTempVRR, vrr.Namespace, vrr.Name)
			tempPVCName := hashName(tempPVCNameBase)
			tempPVC := &corev1.PersistentVolumeClaim{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: serviceNS, Name: tempPVCName}, tempPVC); err == nil {
				// Check if it's owned by this VRR
				for _, ownerRef := range tempPVC.OwnerReferences {
					if ownerRef.Kind == "VolumeRestoreRequest" && ownerRef.Name == vrr.Name && ownerRef.UID == vrr.UID {
						if err := r.Delete(ctx, tempPVC); err != nil && !apierrors.IsNotFound(err) {
							l.Error(err, "Failed to delete temporary PVC", "name", tempPVCName)
						}
						break
					}
				}
			}
			// Update status to Ready
			base := vrr.DeepCopy()
			vrr.Status.TargetPVCRef = &storagev1alpha1.ObjectReference{
				Name:      vrr.Spec.TargetPVCName,
				Namespace: vrr.Spec.TargetNamespace,
			}
			vrr.Status.ObservedGeneration = vrr.Generation
			now := metav1.Now()
			vrr.Status.CompletionTimestamp = &now
			setSingleCondition(&vrr.Status.Conditions, metav1.Condition{
				Type:               storagev1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             storagev1alpha1.ConditionReasonCompleted,
				Message:            fmt.Sprintf("PVC %s/%s restored successfully from PV", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName),
				LastTransitionTime: now,
				ObservedGeneration: vrr.Generation,
			})
			// Set TTL annotation when marking as Ready (same Patch as Ready condition)
			// Use retry on conflict to handle concurrent updates
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				current := &storagev1alpha1.VolumeRestoreRequest{}
				if err := r.Get(ctx, client.ObjectKeyFromObject(vrr), current); err != nil {
					return err
				}
				// Apply status changes
				current.Status = vrr.Status
				// Set TTL annotation
				r.setTTLAnnotation(current)
				// Patch both metadata (annotations) and status in the same operation
				return r.Patch(ctx, current, client.MergeFrom(base))
			}); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update VRR with Ready condition and TTL annotation: %w", err)
			}
			l.Info("VolumeRestoreRequest completed (PVC already Bound)", "source", "PersistentVolume", "pvc", vrr.Spec.TargetPVCName)
			return ctrl.Result{}, nil
		}
		// PVC exists but not Bound yet - requeue instead of failing
		// This handles the case where PVC was created by us but status not updated yet
		l.Info("Target PVC exists but not Bound yet, requeuing", "namespace", vrr.Spec.TargetNamespace, "name", vrr.Spec.TargetPVCName, "phase", targetPVC.Status.Phase)
		return ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
	}
	if !apierrors.IsNotFound(err2) {
		return ctrl.Result{}, fmt.Errorf("failed to check target PVC: %w", err2)
	}

	// 4. Check if PV is available (not bound to another PVC)
	if pv.Spec.ClaimRef != nil {
		return r.markFailed(ctx, vrr, storagev1alpha1.ConditionReasonPVBound, fmt.Sprintf("PersistentVolume %s is already bound to PVC %s/%s", pv.Name, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name))
	}

	// 5. Create temporary PVC in service namespace to bind to PV
	// Hash name if it exceeds 63 characters (Kubernetes limit)
	tempPVCNameBase := fmt.Sprintf("%s%s-%s", NamePrefixTempVRR, vrr.Namespace, vrr.Name)
	tempPVCName := hashName(tempPVCNameBase)

	tempPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tempPVCName,
			Namespace: serviceNS,
			// Add ownerRef to VRR for automatic cleanup
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: APIGroupStorageDeckhouse,
					Kind:       "VolumeRestoreRequest",
					Name:       vrr.Name,
					UID:        vrr.UID,
					Controller: func() *bool { b := false; return &b }(), // Not controller, just for GC
				},
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: stringPtrOrNil(pv.Spec.StorageClassName),
			AccessModes:      pv.Spec.AccessModes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: r.getSizeFromPV(pv),
				},
			},
			VolumeName: pv.Name, // Direct reference to PV
		},
	}

	// Check if temp PVC already exists
	if err := r.Get(ctx, client.ObjectKey{Namespace: serviceNS, Name: tempPVCName}, tempPVC); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, tempPVC); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Someone else created it - this is ok, just continue
				l.Info("Temporary PVC already exists, continuing", "name", tempPVCName)
			} else {
				return ctrl.Result{}, fmt.Errorf("failed to create temporary PVC: %w", err)
			}
		} else {
			l.Info("Created temporary PVC", "name", tempPVCName, "namespace", serviceNS)
		}
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get temporary PVC: %w", err)
	}

	// 6. Check if temporary PVC is Bound (requeue if not)
	bound, requeue, err := r.checkPVCBound(ctx, serviceNS, tempPVCName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check temporary PVC: %w", err)
	}
	if !bound {
		return requeue, nil
	}

	// 7. Get the bound PV to extract volume information
	if err := r.Get(ctx, client.ObjectKey{Name: pv.Name}, pv); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get bound PV: %w", err)
	}

	// 8. Validate StorageClass compatibility
	// According to ADR, restore is prohibited if SC=WFFC (WaitForFirstConsumer)
	// According to ADR, cross-SC restore is not supported (VRR gets Incompatible condition)
	if vrr.Spec.StorageClassName != "" {
		targetSC := &storagev1.StorageClass{}
		// StorageClass is read via APIReader (direct API, no cache) since it's cluster-level
		// configuration that doesn't need to be watched or cached.
		// Controllers MUST read StorageClass via APIReader. APIReader is a required dependency.
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: vrr.Spec.StorageClassName}, targetSC); err != nil {
			if apierrors.IsNotFound(err) {
				return r.markFailed(ctx, vrr, storagev1alpha1.ConditionReasonNotFound, fmt.Sprintf("StorageClass %s not found", vrr.Spec.StorageClassName))
			}
			return ctrl.Result{}, fmt.Errorf("failed to get StorageClass: %w", err)
		}

		// Check for WFFC (WaitForFirstConsumer)
		if targetSC.VolumeBindingMode != nil && *targetSC.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer {
			return r.markIncompatible(ctx, vrr, "StorageClass uses WaitForFirstConsumer volume binding mode, restore is not supported")
		}

		// Check for cross-SC restore: compare source PV StorageClass with target StorageClass
		if pv.Spec.StorageClassName != "" && pv.Spec.StorageClassName != vrr.Spec.StorageClassName {
			return r.markIncompatible(ctx, vrr, fmt.Sprintf("Cross-StorageClass restore not supported: source SC=%s, target SC=%s", pv.Spec.StorageClassName, vrr.Spec.StorageClassName))
		}
	}

	// 9. Create target PVC in target namespace
	// According to ADR, external-provisioner creates PV+PVC through CreateVolume(clone/copy fallback for existing PV)
	targetPVC = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vrr.Spec.TargetPVCName,
			Namespace: vrr.Spec.TargetNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: stringPtrOrNil(vrr.Spec.StorageClassName),
			AccessModes:      pv.Spec.AccessModes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: r.getSizeFromPV(pv),
				},
			},
		},
	}

	if err := r.Create(ctx, targetPVC); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to create target PVC: %w", err)
	}
	l.Info("Created target PVC", "name", vrr.Spec.TargetPVCName, "namespace", vrr.Spec.TargetNamespace)

	// 10. Check if target PVC is Bound (requeue if not)
	bound, requeue, err = r.checkPVCBound(ctx, vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check target PVC: %w", err)
	}
	if !bound {
		return requeue, nil
	}

	// 11. Cleanup temporary PVC in service namespace
	if err := r.Delete(ctx, tempPVC); err != nil && !apierrors.IsNotFound(err) {
		l.Error(err, "Failed to delete temporary PVC", "name", tempPVCName)
		// Don't fail the restore if cleanup fails
	}

	// 12. Update status
	base := vrr.DeepCopy()
	vrr.Status.TargetPVCRef = &storagev1alpha1.ObjectReference{
		Name:      vrr.Spec.TargetPVCName,
		Namespace: vrr.Spec.TargetNamespace,
	}
	vrr.Status.ObservedGeneration = vrr.Generation
	now := metav1.Now()
	vrr.Status.CompletionTimestamp = &now
	setSingleCondition(&vrr.Status.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             storagev1alpha1.ConditionReasonCompleted,
		Message:            fmt.Sprintf("PVC %s/%s restored successfully from PV", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName),
		LastTransitionTime: now,
		ObservedGeneration: vrr.Generation,
	})
	// Set TTL annotation when marking as Ready (same Patch as Ready condition)
	// Use retry on conflict to handle concurrent updates
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeRestoreRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vrr), current); err != nil {
			return err
		}
		// Apply status changes
		current.Status = vrr.Status
		// Set TTL annotation
		r.setTTLAnnotation(current)
		// Patch both metadata (annotations) and status in the same operation
		return r.Patch(ctx, current, client.MergeFrom(base))
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update VRR with Ready condition and TTL annotation: %w", err)
	}

	l.Info("VolumeRestoreRequest completed", "source", "PersistentVolume", "pvc", vrr.Spec.TargetPVCName)
	return ctrl.Result{}, nil
}

// Helper functions

// stringPtrOrNil returns nil if string is empty, otherwise returns pointer to string
// This ensures proper Kubernetes semantics: nil = field not set, "" = explicitly empty
func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (r *VolumeRestoreRequestController) ensureNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			ns = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
			}
			if err := r.Create(ctx, ns); err != nil {
				return fmt.Errorf("failed to create namespace %s: %w", name, err)
			}
		} else {
			return fmt.Errorf("failed to get namespace %s: %w", name, err)
		}
	}
	return nil
}

// hashName creates a deterministic hash of the input string to ensure it fits within 63 characters
// Kubernetes resource names must be <= 63 characters
func hashName(input string) string {
	if len(input) <= 63 {
		return input
	}
	hash := sha256.Sum256([]byte(input))
	hashStr := hex.EncodeToString(hash[:])
	// Use first 8 characters of hash + prefix to ensure uniqueness while staying under 63 chars
	// Keep prefix (NamePrefixTempVRR = 9 chars) + namespace + name prefix + hash (8 chars) = max 63
	prefixLen := 63 - 8 - 1 // 1 for dash, 8 for hash
	if prefixLen < 0 {
		prefixLen = 0
	}
	return fmt.Sprintf("%s-%s", input[:prefixLen], hashStr[:8])
}

func (r *VolumeRestoreRequestController) ensureTemporaryVolumeSnapshot(
	ctx context.Context,
	serviceNS string,
	csiVSC *snapshotv1.VolumeSnapshotContent,
	vrr *storagev1alpha1.VolumeRestoreRequest,
) (string, error) {
	l := log.FromContext(ctx).WithValues("serviceNS", serviceNS, "csiVSC", csiVSC.Name)
	// Generate deterministic name
	csiVSName := fmt.Sprintf("%s%s", NamePrefixVRRTempVS, string(csiVSC.UID))

	// Check if already exists
	csiVS := &snapshotv1.VolumeSnapshot{}
	err8 := r.Get(ctx, client.ObjectKey{Namespace: serviceNS, Name: csiVSName}, csiVS)
	if err8 == nil {
		return csiVSName, nil
	}
	if !apierrors.IsNotFound(err8) {
		return "", fmt.Errorf("failed to check temporary VolumeSnapshot: %w", err8)
	}

	// Re-read VSC to ensure UID is available before creating VS
	// This prevents race condition where VS is created before VSC UID is known
	if err := r.Get(ctx, client.ObjectKey{Name: csiVSC.Name}, csiVSC); err != nil {
		return "", fmt.Errorf("failed to re-read CSI VolumeSnapshotContent: %w", err)
	}
	if csiVSC.UID == "" {
		return "", fmt.Errorf("CSI VolumeSnapshotContent UID not available yet")
	}

	// Create temporary CSI VolumeSnapshot that references the CSI VolumeSnapshotContent
	// Note: CSI VolumeSnapshot can reference VolumeSnapshotContent directly via Source.VolumeSnapshotContentName
	// Add Deckhouse annotations so snapshot-controller treats it as proxy object
	csiVSCName := csiVSC.Name
	newCSIvs := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      csiVSName,
			Namespace: serviceNS,
			Annotations: map[string]string{
				snapshotmeta.AnnDeckhouseManaged: "true",
			},
			Labels: map[string]string{
				LabelKeyCreatedBy:                LabelValueCreatedByVRR,
				LabelKeyCSIVSCName:               csiVSCName,
				snapshotmeta.LabelDeckhouseProxy: "true",
			},
			// No OwnerReference: VS is temporary/ephemeral and should not be part of GC chain
			// VS cleanup will be handled separately by VRR controller, not through ownerRef cascade
			// This prevents GC issues when VS is deleted before VSC
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{
				VolumeSnapshotContentName: &csiVSCName,
			},
		},
	}

	if err := r.Create(ctx, newCSIvs); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Someone else created it - this is ok, return the name
			l.Info("Temporary VolumeSnapshot already exists", "name", csiVSName)
			return csiVSName, nil
		}
		return "", fmt.Errorf("failed to create temporary VolumeSnapshot: %w", err)
	}

	l.Info("Created temporary CSI VolumeSnapshot", "name", csiVSName, "namespace", serviceNS)
	return csiVSName, nil
}

// checkPVCBound checks if PVC is bound and returns requeue result if not
// According to controller-runtime best practices, we should not block with time.Sleep
// Instead, we return RequeueAfter to let the controller handle it properly
func (r *VolumeRestoreRequestController) checkPVCBound(
	ctx context.Context,
	namespace, name string,
) (bool, ctrl.Result, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pvc); err != nil {
		return false, ctrl.Result{}, fmt.Errorf("failed to get PVC: %w", err)
	}

	if pvc.Status.Phase == corev1.ClaimBound {
		return true, ctrl.Result{}, nil
	}

	// PVC is not bound yet, requeue
	return false, ctrl.Result{RequeueAfter: DefaultRequeueInterval}, nil
}

func (r *VolumeRestoreRequestController) cleanupServiceNamespace(
	ctx context.Context,
	serviceNS string,
	csiVSName string,
) error {
	l := log.FromContext(ctx).WithValues("serviceNS", serviceNS, "csiVSName", csiVSName)
	// Delete temporary CSI VolumeSnapshot
	csiVS := &snapshotv1.VolumeSnapshot{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: serviceNS, Name: csiVSName}, csiVS); err == nil {
		if err := r.Delete(ctx, csiVS); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete temporary VolumeSnapshot: %w", err)
		}
		l.Info("Deleted temporary CSI VolumeSnapshot", "name", csiVSName)
	}
	return nil
}

func (r *VolumeRestoreRequestController) getSizeFromCSIVSC(csiVSC *snapshotv1.VolumeSnapshotContent) resource.Quantity {
	if csiVSC.Status != nil && csiVSC.Status.RestoreSize != nil {
		return *resource.NewQuantity(*csiVSC.Status.RestoreSize, resource.BinarySI)
	}
	// Fallback to default size if not available
	return resource.MustParse("1Gi")
}

func (r *VolumeRestoreRequestController) getSizeFromPV(pv *corev1.PersistentVolume) resource.Quantity {
	if pv.Spec.Capacity != nil {
		if storage, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
			return storage
		}
	}
	return resource.MustParse("1Gi")
}

// setTTLAnnotation sets TTL annotation on the object
// TTL is set when Ready/Failed condition is set, and used for automatic deletion
// TTL comes from configuration (storage-foundation module settings), not from VRR spec
// If annotation already exists, it is not overwritten (idempotent)
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

// checkAndHandleTTL checks if TTL has expired and deletes the object if needed
// Returns (shouldDelete, requeueAfter, error)
func (r *VolumeRestoreRequestController) checkAndHandleTTL(ctx context.Context, vrr *storagev1alpha1.VolumeRestoreRequest) (bool, time.Duration, error) {
	// Check if TTL annotation exists
	ttlStr, hasTTL := vrr.Annotations[AnnotationKeyTTL]
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
		base := vrr.DeepCopy()
		vrr.Status.ObservedGeneration = vrr.Generation
		now := metav1.Now()
		setSingleCondition(&vrr.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             storagev1alpha1.ConditionReasonInvalidTTL,
			Message:            fmt.Sprintf("Invalid TTL annotation format: %s (error: %v)", ttlStr, err),
			LastTransitionTime: now,
			ObservedGeneration: vrr.Generation,
		})
		// Use retry on conflict to handle concurrent updates
		if patchErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &storagev1alpha1.VolumeRestoreRequest{}
			if getErr := r.Get(ctx, client.ObjectKeyFromObject(vrr), current); getErr != nil {
				return getErr
			}
			current.Status = vrr.Status
			return r.Status().Patch(ctx, current, client.MergeFrom(base))
		}); patchErr != nil {
			l.Error(patchErr, "Failed to update status with InvalidTTL condition")
			return false, 0, patchErr
		}
		return false, 0, nil
	}

	// Calculate expiration time: completionTimestamp + TTL
	if vrr.Status.CompletionTimestamp == nil {
		// No completion timestamp yet, TTL hasn't started
		return false, 0, nil
	}

	completionTime := vrr.Status.CompletionTimestamp.Time
	expirationTime := completionTime.Add(ttl)
	now := time.Now()

	// Check if TTL has expired
	if now.After(expirationTime) {
		// TTL expired, delete the object
		// NOTE: VRR deletion is safe: temporary objects (VS/PVC) are cleaned up in handleDeletion.
		// VRR TTL only controls when the request resource is deleted (short-lived, cleanup API noise).
		log.FromContext(ctx).Info("TTL expired, deleting VolumeRestoreRequest",
			"namespace", vrr.Namespace,
			"name", vrr.Name,
			"completionTime", completionTime,
			"expirationTime", expirationTime,
		)
		if err := r.Delete(ctx, vrr); err != nil {
			if apierrors.IsNotFound(err) {
				// Already deleted, that's fine (double-delete is safe)
				return true, 0, nil
			}
			return false, 0, fmt.Errorf("failed to delete expired VolumeRestoreRequest: %w", err)
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

	// Add jitter (±10%) to avoid reconcile flood when multiple VRR expire simultaneously
	// This follows the pattern used by JobController, DeploymentController, etc.
	jitterRange := requeueAfter / 10 // 10% jitter
	jitter := time.Duration(rand.Int63n(int64(2*jitterRange))) - jitterRange
	requeueAfter = requeueAfter + jitter
	if requeueAfter < 30*time.Second {
		requeueAfter = 30 * time.Second // Ensure minimum after jitter
	}

	return false, requeueAfter, nil
}

func (r *VolumeRestoreRequestController) markFailed(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
	reason, message string,
) (ctrl.Result, error) {
	base := vrr.DeepCopy()
	vrr.Status.ObservedGeneration = vrr.Generation
	now := metav1.Now()
	vrr.Status.CompletionTimestamp = &now
	setSingleCondition(&vrr.Status.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: vrr.Generation,
	})
	// Set TTL annotation when marking as Failed (same Patch as Ready condition)
	// Use retry on conflict to handle concurrent updates
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeRestoreRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vrr), current); err != nil {
			return err
		}
		// Apply status changes
		current.Status = vrr.Status
		// Set TTL annotation
		r.setTTLAnnotation(current)
		// Patch both metadata (annotations) and status in the same operation
		return r.Patch(ctx, current, client.MergeFrom(base))
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update VRR with Failed condition and TTL annotation: %w", err)
	}
	return ctrl.Result{}, nil
}

// markIncompatible marks VRR as Incompatible according to ADR
// Used for WFFC and cross-SC restore scenarios
func (r *VolumeRestoreRequestController) markIncompatible(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
	message string,
) (ctrl.Result, error) {
	base := vrr.DeepCopy()
	vrr.Status.ObservedGeneration = vrr.Generation
	now := metav1.Now()
	vrr.Status.CompletionTimestamp = &now
	setSingleCondition(&vrr.Status.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             storagev1alpha1.ConditionReasonIncompatible,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: vrr.Generation,
	})
	// Set TTL annotation when marking as Failed (same Patch as Ready condition)
	// Use retry on conflict to handle concurrent updates
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeRestoreRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vrr), current); err != nil {
			return err
		}
		// Apply status changes
		current.Status = vrr.Status
		// Set TTL annotation
		r.setTTLAnnotation(current)
		// Patch both metadata (annotations) and status in the same operation
		return r.Patch(ctx, current, client.MergeFrom(base))
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update VRR with Failed condition and TTL annotation: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *VolumeRestoreRequestController) handleDeletion(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("vrr", fmt.Sprintf("%s/%s", vrr.Namespace, vrr.Name))

	// Cleanup temporary objects in service namespace
	// OwnerRef should handle this automatically, but we do explicit cleanup for safety
	serviceNS := vrr.Spec.ServiceNamespace
	if serviceNS == "" {
		serviceNS = ServiceNamespace
	}

	// Cleanup temporary VolumeSnapshot (for restore-from-VSC)
	// Find all VS with label created-by=VRR and ownerRef to this VRR
	csiVSList := &snapshotv1.VolumeSnapshotList{}
	if err := r.List(ctx, csiVSList, client.InNamespace(serviceNS), client.MatchingLabels{
		LabelKeyCreatedBy: LabelValueCreatedByVRR,
	}); err == nil {
		for i := range csiVSList.Items {
			vs := &csiVSList.Items[i]
			for _, ownerRef := range vs.OwnerReferences {
				if ownerRef.Kind == "VolumeRestoreRequest" && ownerRef.Name == vrr.Name && ownerRef.UID == vrr.UID {
					if err := r.Delete(ctx, vs); err != nil && !apierrors.IsNotFound(err) {
						l.Error(err, "Failed to delete temporary VolumeSnapshot", "name", vs.Name)
					} else {
						l.Info("Deleted temporary VolumeSnapshot", "name", vs.Name)
					}
					break
				}
			}
		}
	}

	// Cleanup temporary PVC (for restore-from-PV)
	// Find all PVC with ownerRef to this VRR
	tempPVCNameBase := fmt.Sprintf("%s%s-%s", NamePrefixTempVRR, vrr.Namespace, vrr.Name)
	tempPVCName := hashName(tempPVCNameBase)
	tempPVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: serviceNS, Name: tempPVCName}, tempPVC); err == nil {
		// Check if it's owned by this VRR
		for _, ownerRef := range tempPVC.OwnerReferences {
			if ownerRef.Kind == "VolumeRestoreRequest" && ownerRef.Name == vrr.Name && ownerRef.UID == vrr.UID {
				if err := r.Delete(ctx, tempPVC); err != nil && !apierrors.IsNotFound(err) {
					l.Error(err, "Failed to delete temporary PVC", "name", tempPVCName)
				} else {
					l.Info("Deleted temporary PVC", "name", tempPVCName)
				}
				break
			}
		}
	}

	// Note: We don't delete the target PVC on VRR deletion
	// The target PVC should be managed by the user or higher-level controller
	return ctrl.Result{}, nil
}

func (r *VolumeRestoreRequestController) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.VolumeRestoreRequest{}).
		Complete(r)
}
