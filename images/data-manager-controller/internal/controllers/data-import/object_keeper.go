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

package dataimport

import (
	"context"
	"fmt"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// ObjectKeeper contract (deckhouse.io; talked to via unstructured to avoid a Go dep).
//
// Why DataImport keeps its own keeper even though storage-foundation already retains the artifact:
// the VCR controller owns the produced VSC/PV via a retainer ObjectKeeper that FollowObject-follows the
// *VCR*, and the VCR garbage collector deletes a completed VCR at CompletionTimestamp + GC_VCR_TTL
// (default 24h), cascading GC to that retainer keeper and the artifact object. So the storage-foundation
// keeper only anchors the artifact to the VCR's (bounded) lifetime, not to the import's. DataImport adds
// its own cluster-scoped keeper (FollowObject -> DataImport) as an ADDITIONAL, non-controller owner of
// the artifact so the artifact's Kubernetes object survives the VCR TTL until the state-snapshotter
// import orchestrator (C5) adopts it under a SnapshotContent (the durable owner). Multi-owner GC keeps
// the artifact alive while ANY owner remains. We never become the controller owner and never remove the
// storage-foundation owner — the Retain pin (artifact.go) additionally protects the physical data.
const (
	objectKeeperKind             = "ObjectKeeper"
	objectKeeperModeFollowObject = "FollowObject"
)

var objectKeeperGVR = schema.GroupVersionResource{
	Group:    "deckhouse.io",
	Version:  "v1alpha1",
	Resource: "objectkeepers",
}

func objectKeeperName(uid types.UID) string {
	return "data-import-" + string(uid)
}

// ensureObjectKeeper idempotently ensures the import ObjectKeeper for this DataImport exists with a
// populated UID (a hard barrier: the UID is required before the keeper can be used as an ownerReference).
// Returns a requeue Result while the UID is not yet observable.
func (r *DataImportReconciler) ensureObjectKeeper(ctx context.Context) (*unstructured.Unstructured, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if r.dataImport.UID == "" {
		return nil, ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	}

	name := objectKeeperName(r.dataImport.UID)
	cli := r.Dynamic.Resource(objectKeeperGVR)

	keeper, err := cli.Get(ctx, name, metav1.GetOptions{})
	switch {
	case kubeerrors.IsNotFound(err):
		created, createErr := cli.Create(ctx, buildObjectKeeper(name, r.dataImport), metav1.CreateOptions{})
		if createErr != nil && !kubeerrors.IsAlreadyExists(createErr) {
			return nil, ctrl.Result{}, fmt.Errorf("create ObjectKeeper %s: %w", name, createErr)
		}
		logger.Info("Created import ObjectKeeper", "name", name, "dataImport", r.dataImport.Name)
		if createErr == nil {
			keeper = created
		} else {
			keeper, err = cli.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
			}
		}
	case err != nil:
		return nil, ctrl.Result{}, fmt.Errorf("get ObjectKeeper %s: %w", name, err)
	default:
		if validateErr := validateObjectKeeperOwnership(name, keeper, r.dataImport); validateErr != nil {
			return nil, ctrl.Result{}, validateErr
		}
	}

	if keeper.GetUID() == "" {
		logger.Info("ObjectKeeper UID not assigned yet, requeuing", "name", name)
		return nil, ctrl.Result{RequeueAfter: dataImportRequeueInterval}, nil
	}
	return keeper, ctrl.Result{}, nil
}

func buildObjectKeeper(name string, di *dev1alpha1.DataImport) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": objectKeeperGVR.Group + "/" + objectKeeperGVR.Version,
		"kind":       objectKeeperKind,
		"metadata": map[string]interface{}{
			"name": name,
		},
		"spec": map[string]interface{}{
			"mode": objectKeeperModeFollowObject,
			"followObjectRef": map[string]interface{}{
				"apiVersion": dev1alpha1.SchemeGroupVersion.String(),
				"kind":       dataImportKind,
				"namespace":  di.Namespace,
				"name":       di.Name,
				"uid":        string(di.UID),
			},
		},
	}}
}

// validateObjectKeeperOwnership guards against reusing a keeper that does not match this DataImport
// (name collision, hand-edit, or DataImport recreated with the same name but a new UID).
func validateObjectKeeperOwnership(name string, keeper *unstructured.Unstructured, di *dev1alpha1.DataImport) error {
	mode, _, _ := unstructured.NestedString(keeper.Object, "spec", "mode")
	if mode != objectKeeperModeFollowObject {
		return fmt.Errorf("ObjectKeeper %s has unexpected mode %q (expected %q)", name, mode, objectKeeperModeFollowObject)
	}
	ref, found, _ := unstructured.NestedStringMap(keeper.Object, "spec", "followObjectRef")
	if !found {
		return fmt.Errorf("ObjectKeeper %s has no followObjectRef", name)
	}
	wantUID := string(di.UID)
	if ref["kind"] != dataImportKind || ref["namespace"] != di.Namespace || ref["name"] != di.Name || ref["uid"] != wantUID {
		return fmt.Errorf("ObjectKeeper %s belongs to another object (followObjectRef mismatch: expected DataImport %s/%s uid %s, got %s/%s uid %s)",
			name, di.Namespace, di.Name, wantUID, ref["namespace"], ref["name"], ref["uid"])
	}
	return nil
}

// ensureArtifactKeptByKeeper adds the import ObjectKeeper as an ADDITIONAL, non-controller owner of the
// produced artifact (idempotent). It never touches the existing controller owner (storage-foundation's
// VCR retainer) and never becomes the controller itself — it only appends, so the artifact gains a
// second owner anchored to the DataImport's lifetime.
func (r *DataImportReconciler) ensureArtifactKeptByKeeper(ctx context.Context, artifact *dev1alpha1.DataArtifactReference, keeper *unstructured.Unstructured) error {
	desired := keeperOwnerReference(keeper)
	switch artifact.Kind {
	case artifactKindVolumeSnapshotContent:
		return r.appendOwnerRef(ctx, artifact.Name, desired, func() client.Object { return &snapv1.VolumeSnapshotContent{} })
	case artifactKindPersistentVolume:
		return r.appendOwnerRef(ctx, artifact.Name, desired, func() client.Object { return &corev1.PersistentVolume{} })
	default:
		return fmt.Errorf("cannot keep unsupported artifact kind %q", artifact.Kind)
	}
}

// appendOwnerRef appends the desired non-controller ownerReference to the named cluster-scoped artifact
// if absent. It re-reads under RetryOnConflict and patches with an optimistic lock so a concurrent
// ownerReferences writer (e.g. the C5 import orchestrator adding the SnapshotContent owner) can never be
// clobbered — the loser re-reads and re-merges instead of dropping the other's owner.
func (r *DataImportReconciler) appendOwnerRef(ctx context.Context, name string, desired metav1.OwnerReference, newObj func() client.Object) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := newObj()
		if err := r.Client.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
			return fmt.Errorf("get artifact %s: %w", name, err)
		}
		for _, ref := range obj.GetOwnerReferences() {
			if desired.UID != "" && ref.UID == desired.UID {
				return nil
			}
		}
		base := obj.DeepCopyObject().(client.Object)
		obj.SetOwnerReferences(append(obj.GetOwnerReferences(), desired))
		if err := r.Client.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		return nil
	})
}

// keeperOwnerReference builds a NON-controller ownerReference to the keeper (Controller intentionally
// unset: storage-foundation's VCR retainer is the single controller owner).
func keeperOwnerReference(keeper *unstructured.Unstructured) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: objectKeeperGVR.Group + "/" + objectKeeperGVR.Version,
		Kind:       objectKeeperKind,
		Name:       keeper.GetName(),
		UID:        keeper.GetUID(),
	}
}
