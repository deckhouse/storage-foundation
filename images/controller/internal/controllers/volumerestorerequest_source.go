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

package controllers

import (
	"context"
	"fmt"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

var snapshotContentGVK = schema.GroupVersionKind{
	Group:   "state-snapshotter.deckhouse.io",
	Version: "v1alpha1",
	Kind:    "SnapshotContent",
}

type sourceTrace struct {
	want    string
	anchor  string
	owner   *metav1.OwnerReference
	ownerNS string
}

func (r *VolumeRestoreRequestController) sourceOwnerNamespace(ctx context.Context, owners []metav1.OwnerReference) (*metav1.OwnerReference, string, error) {
	ref := metav1.GetControllerOfNoCopy(&metav1.ObjectMeta{OwnerReferences: owners})
	if ref == nil {
		return nil, "", nil
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return ref, "", nil
	}

	switch {
	case gv.Group == deckhousev1alpha1.SchemeGroupVersion.Group && ref.Kind == KindObjectKeeper:
		keeper := &deckhousev1alpha1.ObjectKeeper{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: ref.Name}, keeper); err != nil {
			if apierrors.IsNotFound(err) {
				return ref, "", nil
			}
			return ref, "", fmt.Errorf("failed to get ObjectKeeper %s: %w", ref.Name, err)
		}
		if keeper.UID != ref.UID || keeper.Spec.FollowObjectRef == nil {
			return ref, "", nil
		}
		return ref, keeper.Spec.FollowObjectRef.Namespace, nil
	case gv.Group == snapshotContentGVK.Group && ref.Kind == snapshotContentGVK.Kind:
		content := &unstructured.Unstructured{}
		content.SetGroupVersionKind(snapshotContentGVK)
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: ref.Name}, content); err != nil {
			if apierrors.IsNotFound(err) {
				return ref, "", nil
			}
			return ref, "", fmt.Errorf("failed to get SnapshotContent %s: %w", ref.Name, err)
		}
		if content.GetUID() != ref.UID {
			return ref, "", nil
		}
		ns, _, _ := unstructured.NestedString(content.Object, "spec", "snapshotRef", "namespace")
		if ns == "" {
			ns, _, _ = unstructured.NestedString(content.Object, "status", "data", "sourceRef", "namespace")
		}
		return ref, ns, nil
	}
	return ref, "", nil
}

func (r *VolumeRestoreRequestController) verifySource(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
	source client.Object,
	anchorNamespace string,
) (bool, error) {
	trace := sourceTrace{want: vrr.Namespace, anchor: anchorNamespace}
	reason := storagev1alpha1.ConditionReasonSourceInNamespace
	if r.Config != nil && vrr.Namespace == r.Config.ControllerNamespace {
		reason = storagev1alpha1.ConditionReasonModuleNamespace
		trace.want = vrr.Annotations[storagev1alpha1.AnnotationStorageManagerNamespaceKey]
		if trace.want == "" {
			return r.rejectForeignSource(ctx, vrr, source, trace)
		}
	}

	switch {
	case anchorNamespace == trace.want:
	case anchorNamespace != "":
		return r.rejectForeignSource(ctx, vrr, source, trace)
	default:
		owner, ns, err := r.sourceOwnerNamespace(ctx, source.GetOwnerReferences())
		if err != nil {
			return true, err
		}
		trace.owner, trace.ownerNS = owner, ns
		if ns != trace.want {
			return r.rejectForeignSource(ctx, vrr, source, trace)
		}
	}

	if err := r.setSourceVerified(ctx, vrr, reason); err != nil {
		return true, err
	}
	return false, nil
}

func (r *VolumeRestoreRequestController) rejectForeignSource(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
	source client.Object,
	trace sourceTrace,
) (bool, error) {
	owner := ""
	if trace.owner != nil {
		owner = fmt.Sprintf("%s %s/%s", trace.owner.APIVersion, trace.owner.Kind, trace.owner.Name)
	}
	log.FromContext(ctx).Info("Restore source does not belong to the namespace of the request",
		"vrr", fmt.Sprintf("%s/%s", vrr.Namespace, vrr.Name),
		"sourceKind", vrr.Spec.SourceRef.Kind, "source", source.GetName(),
		"wantNamespace", trace.want, "anchorNamespace", trace.anchor,
		"owner", owner, "ownerNamespace", trace.ownerNS)
	message := fmt.Sprintf("%s %s does not belong to namespace %s", vrr.Spec.SourceRef.Kind, source.GetName(), trace.want)
	if trace.want == "" {
		message = fmt.Sprintf("%s %s cannot be verified: the request has no %s annotation",
			vrr.Spec.SourceRef.Kind, source.GetName(), storagev1alpha1.AnnotationStorageManagerNamespaceKey)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(vrr, nil, corev1.EventTypeWarning, storagev1alpha1.ConditionReasonForeignSource, "VerifySource", "%s", message)
	}
	meta.RemoveStatusCondition(&vrr.Status.Conditions, storagev1alpha1.ConditionTypeSourceVerified)
	_, err := r.markFailed(ctx, vrr, storagev1alpha1.ConditionReasonForeignSource, message)
	return true, err
}

func (r *VolumeRestoreRequestController) setSourceVerified(
	ctx context.Context,
	vrr *storagev1alpha1.VolumeRestoreRequest,
	reason string,
) error {
	cond := metav1.Condition{
		Type:               storagev1alpha1.ConditionTypeSourceVerified,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            fmt.Sprintf("%s %s may be restored into namespace %s", vrr.Spec.SourceRef.Kind, vrr.Spec.SourceRef.Name, vrr.Namespace),
		ObservedGeneration: vrr.Generation,
	}
	if existing := meta.FindStatusCondition(vrr.Status.Conditions, cond.Type); existing != nil &&
		existing.Status == cond.Status && existing.Reason == cond.Reason && existing.ObservedGeneration == cond.ObservedGeneration {
		return nil
	}

	base := vrr.DeepCopy()
	meta.SetStatusCondition(&vrr.Status.Conditions, cond)
	if err := r.Status().Patch(ctx, vrr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to set %s condition: %w", cond.Type, err)
	}
	return nil
}
