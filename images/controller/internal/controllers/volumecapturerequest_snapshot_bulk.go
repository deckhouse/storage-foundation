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
	"strings"
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

func shortTargetUID(targetUID string) string {
	compact := strings.ReplaceAll(targetUID, "-", "")
	if len(compact) <= 8 {
		return compact
	}
	return compact[len(compact)-8:]
}

func snapshotVSCName(vcrUID types.UID, targetUID string) string {
	return fmt.Sprintf("snapshot-%s-%s", string(vcrUID), shortTargetUID(targetUID))
}

func validateSnapshotTargets(targets []storagev1alpha1.VolumeCaptureTarget) error {
	if len(targets) == 0 {
		return fmt.Errorf("spec.targets must not be empty for Snapshot mode")
	}
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		if t.UID == "" {
			return fmt.Errorf("target uid must not be empty")
		}
		if _, dup := seen[t.UID]; dup {
			return fmt.Errorf("duplicate target uid %q", t.UID)
		}
		seen[t.UID] = struct{}{}
	}
	return nil
}

func volumeSnapshotBinding(target storagev1alpha1.VolumeCaptureTarget, vscName string) storagev1alpha1.VolumeDataBinding {
	return storagev1alpha1.VolumeDataBinding{
		TargetUID: target.UID,
		Target:    target,
		Artifact: storagev1alpha1.VolumeDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1",
			Kind:       "VolumeSnapshotContent",
			Name:       vscName,
		},
	}
}

func upsertVolumeDataBinding(bindings []storagev1alpha1.VolumeDataBinding, binding storagev1alpha1.VolumeDataBinding) []storagev1alpha1.VolumeDataBinding {
	for i := range bindings {
		if bindings[i].TargetUID == binding.TargetUID {
			bindings[i] = binding
			return bindings
		}
	}
	return append(bindings, binding)
}

func (r *VolumeCaptureRequestController) patchVCRSnapshotProgress(
	ctx context.Context,
	vcr *storagev1alpha1.VolumeCaptureRequest,
	dataRefs []storagev1alpha1.VolumeDataBinding,
	readyCount, total int,
) error {
	message := fmt.Sprintf("%d of %d targets ready", readyCount, total)
	now := metav1.Now()
	desired := vcr.Status.DeepCopy()
	desired.DataRefs = dataRefs
	setSingleCondition(&desired.Conditions, metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             storagev1alpha1.ConditionReasonTargetsPending,
		Message:            message,
		LastTransitionTime: now,
	})
	desired.CompletionTimestamp = nil

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &storagev1alpha1.VolumeCaptureRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(vcr), current); err != nil {
			return err
		}
		base := current.DeepCopy()
		current.Status.DataRefs = desired.DataRefs
		current.Status.Conditions = desired.Conditions
		current.Status.CompletionTimestamp = nil
		return r.Status().Patch(ctx, current, client.MergeFrom(base))
	})
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
		}}, ctrl.Result{}, nil
	}

	if csiVSC.Status == nil || csiVSC.Status.ReadyToUse == nil || !*csiVSC.Status.ReadyToUse {
		l.Info("Waiting for external-snapshotter to set ReadyToUse=true", "name", csiVSCName)
		return snapshotTargetResult{pending: true}, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	binding := volumeSnapshotBinding(target, csiVSCName)
	return snapshotTargetResult{
		ready:   true,
		binding: &binding,
	}, ctrl.Result{}, nil
}

func (r *VolumeCaptureRequestController) markFailedSnapshotForTarget(
	ctx context.Context,
	vcr *storagev1alpha1.VolumeCaptureRequest,
	target storagev1alpha1.VolumeCaptureTarget,
	vscName, reason, message string,
) (ctrl.Result, error) {
	if vscName != "" {
		binding := volumeSnapshotBinding(target, vscName)
		vcr.Status.DataRefs = upsertVolumeDataBinding(vcr.Status.DataRefs, binding)
	}
	if err := r.finalizeVCR(ctx, vcr, metav1.ConditionFalse, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
