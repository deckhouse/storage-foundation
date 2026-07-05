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
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

type snapshotTargetError struct {
	target  storagev1alpha1.VolumeCaptureTarget
	reason  string
	message string
	vscName string
	vscUID  string
}

type snapshotTargetResult struct {
	binding  *storagev1alpha1.VolumeDataBinding
	ready    bool
	pending  bool
	terminal *snapshotTargetError
}

func objectKeeperNameForVCR(vcrUID types.UID) string {
	return NamePrefixRetainerVCR + string(vcrUID)
}

const snapshotTargetHashHexLen = 12

// targetUIDHash returns a stable short hash of target UID for VSC naming (avoids suffix collisions).
func targetUIDHash(targetUID string) string {
	sum := sha256.Sum256([]byte(targetUID))
	return hex.EncodeToString(sum[:])[:snapshotTargetHashHexLen]
}

func snapshotVSCName(vcrUID types.UID, targetUID string) string {
	return fmt.Sprintf("snapshot-%s-%s", string(vcrUID), targetUIDHash(targetUID))
}

func volumeSnapshotBinding(_ storagev1alpha1.VolumeCaptureTarget, vscName, vscUID string) storagev1alpha1.VolumeDataBinding {
	return storagev1alpha1.VolumeDataBinding{
		Artifact: storagev1alpha1.VolumeDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1",
			Kind:       "VolumeSnapshotContent",
			Name:       vscName,
			// UID is best-effort: empty when the artifact is referenced before its object is known.
			UID: vscUID,
		},
	}
}

// patchVCRSnapshotPending surfaces a non-terminal "capture in progress" Ready=False/TargetsPending
// condition while the single target is still being captured. It never sets dataRef (only success does).
func (r *VolumeCaptureRequestController) patchVCRSnapshotPending(
	ctx context.Context,
	vcr *storagev1alpha1.VolumeCaptureRequest,
) error {
	now := metav1.Now()
	pendingCondition := metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             storagev1alpha1.ConditionReasonTargetsPending,
		Message:            "target capture in progress",
		LastTransitionTime: now,
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeCaptureRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); err != nil {
			return err
		}
		base := current.DeepCopy()
		setSingleCondition(&current.Status.Conditions, pendingCondition)
		current.Status.CompletionTimestamp = nil
		return r.Status().Patch(ctx, current, client.MergeFrom(base))
	})
}

// requeueIfObjectKeeperUIDEmpty re-reads ObjectKeeper; returns RequeueAfter=1s when UID is not yet assigned.
func (r *VolumeCaptureRequestController) requeueIfObjectKeeperUIDEmpty(
	ctx context.Context,
	retainerName string,
) (*deckhousev1alpha1.ObjectKeeper, ctrl.Result, error) {
	objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
	if err := r.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper); err != nil {
		return nil, ctrl.Result{}, err
	}
	if objectKeeper.UID == "" {
		return nil, ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return objectKeeper, ctrl.Result{}, nil
}

func (r *VolumeCaptureRequestController) processSnapshotTarget(
	ctx context.Context,
	vcr *storagev1alpha1.VolumeCaptureRequest,
	objectKeeper *deckhousev1alpha1.ObjectKeeper,
	retainerName string,
	target storagev1alpha1.VolumeCaptureTarget,
) (snapshotTargetResult, ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues(
		"targetUID", target.UID,
		"pvc", fmt.Sprintf("%s/%s", target.Namespace, target.Name),
	)

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return snapshotTargetResult{terminal: &snapshotTargetError{
				target: target, reason: storagev1alpha1.ConditionReasonNotFound,
				message: fmt.Sprintf("PVC %s/%s not found", target.Namespace, target.Name),
			}}, ctrl.Result{}, nil
		}
		if apierrors.IsForbidden(err) {
			return snapshotTargetResult{terminal: &snapshotTargetError{
				target: target, reason: storagev1alpha1.ConditionReasonRBACDenied,
				message: fmt.Sprintf("Access denied to PVC %s/%s", target.Namespace, target.Name),
			}}, ctrl.Result{}, nil
		}
		return snapshotTargetResult{}, ctrl.Result{}, fmt.Errorf("failed to get PVC: %w", err)
	}

	if pvc.Spec.VolumeName == "" {
		return snapshotTargetResult{terminal: &snapshotTargetError{
			target: target, reason: storagev1alpha1.ConditionReasonInternalError,
			message: fmt.Sprintf("PVC %s/%s is not bound", pvc.Namespace, pvc.Name),
		}}, ctrl.Result{}, nil
	}

	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return snapshotTargetResult{terminal: &snapshotTargetError{
				target: target, reason: storagev1alpha1.ConditionReasonNotFound,
				message: fmt.Sprintf("PV %s not found", pvc.Spec.VolumeName),
			}}, ctrl.Result{}, nil
		}
		return snapshotTargetResult{}, ctrl.Result{}, fmt.Errorf("failed to get PV: %w", err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return snapshotTargetResult{terminal: &snapshotTargetError{
			target: target, reason: storagev1alpha1.ConditionReasonInternalError,
			message: fmt.Sprintf("PV %s does not have CSI volume handle", pv.Name),
		}}, ctrl.Result{}, nil
	}

	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return snapshotTargetResult{terminal: &snapshotTargetError{
			target: target, reason: storagev1alpha1.ConditionReasonInternalError,
			message: "PVC does not have StorageClassName",
		}}, ctrl.Result{}, nil
	}
	storageClassName := *pvc.Spec.StorageClassName
	storageClass := &storagev1.StorageClass{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: storageClassName}, storageClass); err != nil {
		if apierrors.IsNotFound(err) {
			return snapshotTargetResult{terminal: &snapshotTargetError{
				target: target, reason: storagev1alpha1.ConditionReasonNotFound,
				message: fmt.Sprintf("StorageClass %s not found", storageClassName),
			}}, ctrl.Result{}, nil
		}
		if apierrors.IsForbidden(err) {
			return snapshotTargetResult{terminal: &snapshotTargetError{
				target: target, reason: storagev1alpha1.ConditionReasonRBACDenied,
				message: fmt.Sprintf("Access denied to StorageClass %s", storageClassName),
			}}, ctrl.Result{}, nil
		}
		return snapshotTargetResult{}, ctrl.Result{}, fmt.Errorf("failed to get StorageClass: %w", err)
	}

	vscClassName, ok := storageClass.Annotations["storage.deckhouse.io/volumesnapshotclass"]
	if !ok || vscClassName == "" {
		return snapshotTargetResult{terminal: &snapshotTargetError{
			target: target, reason: storagev1alpha1.ConditionReasonNotFound,
			message: fmt.Sprintf("StorageClass %s does not have storage.deckhouse.io/volumesnapshotclass annotation", storageClassName),
		}}, ctrl.Result{}, nil
	}

	vscClass := &snapshotv1.VolumeSnapshotClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: vscClassName}, vscClass); err != nil {
		if apierrors.IsNotFound(err) {
			return snapshotTargetResult{terminal: &snapshotTargetError{
				target: target, reason: storagev1alpha1.ConditionReasonNotFound,
				message: fmt.Sprintf("VolumeSnapshotClass %s (from StorageClass %s annotation) not found", vscClassName, storageClassName),
			}}, ctrl.Result{}, nil
		}
		if apierrors.IsForbidden(err) {
			return snapshotTargetResult{terminal: &snapshotTargetError{
				target: target, reason: storagev1alpha1.ConditionReasonRBACDenied,
				message: fmt.Sprintf("Access denied to VolumeSnapshotClass %s", vscClassName),
			}}, ctrl.Result{}, nil
		}
		return snapshotTargetResult{}, ctrl.Result{}, fmt.Errorf("failed to get VolumeSnapshotClass: %w", err)
	}
	if vscClass.Driver != pv.Spec.CSI.Driver {
		return snapshotTargetResult{terminal: &snapshotTargetError{
			target: target, reason: storagev1alpha1.ConditionReasonInternalError,
			message: fmt.Sprintf("VolumeSnapshotClass %s driver %s does not match PV CSI driver %s", vscClassName, vscClass.Driver, pv.Spec.CSI.Driver),
		}}, ctrl.Result{}, nil
	}

	csiVSCName := snapshotVSCName(vcr.UID, target.UID)
	csiVSC := &snapshotv1.VolumeSnapshotContent{}
	err := r.Get(ctx, client.ObjectKey{Name: csiVSCName}, csiVSC)
	if apierrors.IsNotFound(err) {
		freshKeeper, requeueResult, err := r.requeueIfObjectKeeperUIDEmpty(ctx, retainerName)
		if err != nil {
			return snapshotTargetResult{}, ctrl.Result{}, err
		}
		if requeueResult.Requeue || requeueResult.RequeueAfter > 0 {
			l.Info("ObjectKeeper UID not assigned yet, requeue before creating VSC", "retainer", retainerName)
			return snapshotTargetResult{pending: true}, requeueResult, nil
		}
		objectKeeper = freshKeeper

		volumeHandle := pv.Spec.CSI.VolumeHandle
		vscClassName := vscClass.Name
		controllerTrue := true
		blockOwnerDeletion := true
		csiVSC = &snapshotv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name: csiVSCName,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         APIGroupDeckhouse,
					Kind:               KindObjectKeeper,
					Name:               retainerName,
					UID:                objectKeeper.UID,
					Controller:         &controllerTrue,
					BlockOwnerDeletion: &blockOwnerDeletion,
				}},
			},
			Spec: snapshotv1.VolumeSnapshotContentSpec{
				Driver:                  vscClass.Driver,
				VolumeSnapshotClassName: &vscClassName,
				DeletionPolicy:          vscClass.DeletionPolicy,
				Source: snapshotv1.VolumeSnapshotContentSource{
					VolumeHandle: &volumeHandle,
				},
				VolumeSnapshotRef: corev1.ObjectReference{},
			},
		}
		if err := r.Create(ctx, csiVSC); err != nil {
			return snapshotTargetResult{}, ctrl.Result{}, fmt.Errorf("failed to create VolumeSnapshotContent: %w", err)
		}
		l.Info("Created VolumeSnapshotContent", "name", csiVSCName)
		return snapshotTargetResult{pending: true}, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err != nil {
		return snapshotTargetResult{}, ctrl.Result{}, fmt.Errorf("failed to get VolumeSnapshotContent: %w", err)
	}

	if csiVSC.Status != nil && csiVSC.Status.Error != nil && csiVSC.Status.Error.Message != nil && *csiVSC.Status.Error.Message != "" {
		errorMsg := *csiVSC.Status.Error.Message
		errorDetails := fmt.Sprintf("VSC %s, PVC %s/%s: %s", csiVSCName, pvc.Namespace, pvc.Name, errorMsg)
		if csiVSC.Status.Error.Time != nil {
			errorDetails = fmt.Sprintf("%s (error time: %s)", errorDetails, csiVSC.Status.Error.Time.Format(time.RFC3339))
		}
		return snapshotTargetResult{terminal: &snapshotTargetError{
			target: target, reason: storagev1alpha1.ConditionReasonSnapshotCreationFailed,
			message: fmt.Sprintf("CSI snapshot creation failed: %s", errorDetails),
			vscName: csiVSCName,
			vscUID:  string(csiVSC.UID),
		}}, ctrl.Result{}, nil
	}

	if csiVSC.Status == nil || csiVSC.Status.ReadyToUse == nil || !*csiVSC.Status.ReadyToUse {
		l.Info("Waiting for external-snapshotter to set ReadyToUse=true", "name", csiVSCName)
		return snapshotTargetResult{pending: true}, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	binding := volumeSnapshotBinding(target, csiVSCName, string(csiVSC.UID))
	return snapshotTargetResult{
		ready:   true,
		binding: &binding,
	}, ctrl.Result{}, nil
}

func (r *VolumeCaptureRequestController) markFailedSnapshotForTarget(
	ctx context.Context,
	vcr *storagev1alpha1.VolumeCaptureRequest,
	target storagev1alpha1.VolumeCaptureTarget,
	vscName, vscUID, reason, message string,
) (ctrl.Result, error) {
	if vscName != "" {
		binding := volumeSnapshotBinding(target, vscName, vscUID)
		vcr.Status.Data = &binding
	}
	if err := r.finalizeVCR(ctx, vcr, metav1.ConditionFalse, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
