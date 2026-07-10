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

// Package vrr_provisioner_rbac grants the patched csi-provisioner VRR executor the cluster-wide
// RBAC it needs. The executor (Deckhouse fork branch d8-63742164-vrr of external-provisioner) runs
// as a sidecar inside each backend CSI driver controller Pod under ServiceAccount "csi" and starts a
// dynamic informer on volumerestorerequests across ALL namespaces. A namespaced Role is therefore
// insufficient: the SA needs a ClusterRole + ClusterRoleBinding with get/list/watch on
// volumerestorerequests. storage-foundation builds that sidecar image but the backend driver modules
// do not ship this grant, so we reconcile it here for a hardcoded list of module namespaces.
//
// The executor needs two net-new grants on top of a backend driver's stock provisioner role:
//   - volumerestorerequests get/list/watch: it watches VRRs from a cluster-wide informer and never
//     writes status (status is owned by the storage-foundation VRR controller).
//   - persistentvolumeclaims create (plus get/list/watch/update/patch): on a successful restore the
//     executor creates the target PVC and binds it to the PV. The stock external-provisioner only
//     creates PVs, so driver roles grant PVC get/list/watch/update but NOT create; without this the
//     restore stalls with "persistentvolumeclaims is forbidden ... cannot create".
//
// PV/VolumeSnapshotContent/StorageClass/Secrets/Events are intentionally NOT granted here: the
// driver's existing provisioner role already covers them for normal provisioning (the executor's
// CSI CreateVolume + PV creation + event emission already succeed). VolumeCaptureRequest is also
// excluded - it is handled by the storage-foundation controller, not by any csi-side sidecar.
package hooks_common

import (
	"context"
	"errors"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/module-sdk/pkg"
	"github.com/deckhouse/module-sdk/pkg/registry"
	"github.com/deckhouse/sds-common-lib/kubeclient"
	"github.com/deckhouse/storage-foundation/hooks/go/consts"
)

const hookName = "vrr-provisioner-rbac"

// Apply on every module run so the grant exists before the CSI driver workloads start and is
// re-converged after manual edits. Cleanup on module delete because the ClusterRole and the
// cluster-scoped bindings live outside the module namespace and are not pruned by Helm.
var _ = registry.RegisterFunc(
	&pkg.HookConfig{OnBeforeHelm: &pkg.OrderedConfig{Order: 10}},
	handlerApply,
)

var _ = registry.RegisterFunc(
	&pkg.HookConfig{OnAfterDeleteHelm: &pkg.OrderedConfig{Order: 10}},
	handlerCleanup,
)

func newClient() (client.Client, error) {
	cl, err := kubeclient.New(clientgoscheme.AddToScheme)
	if err != nil {
		return nil, fmt.Errorf("[%s]: failed to initialize kube client: %w", hookName, err)
	}
	return cl, nil
}

func handlerApply(ctx context.Context, input *pkg.HookInput) error {
	input.Logger.Info(fmt.Sprintf("[%s]: ensuring VRR executor RBAC", hookName))

	cl, err := newClient()
	if err != nil {
		return err
	}

	if err := applyClusterRole(ctx, cl); err != nil {
		return err
	}

	var resultErr error
	for _, namespace := range consts.VRRExecutorNamespaces {
		if err := applyClusterRoleBinding(ctx, cl, namespace); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}

	input.Logger.Info(fmt.Sprintf("[%s]: VRR executor RBAC reconciled for %d namespace(s)", hookName, len(consts.VRRExecutorNamespaces)))
	return resultErr
}

func handlerCleanup(ctx context.Context, input *pkg.HookInput) error {
	input.Logger.Info(fmt.Sprintf("[%s]: removing VRR executor RBAC on module delete", hookName))

	cl, err := newClient()
	if err != nil {
		return err
	}

	var resultErr error
	for _, namespace := range consts.VRRExecutorNamespaces {
		crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: bindingName(namespace)}}
		if err := cl.Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
			resultErr = errors.Join(resultErr, fmt.Errorf("[%s]: delete ClusterRoleBinding %q: %w", hookName, crb.Name, err))
		}
	}

	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: consts.VRRProvisionerExecutorClusterRoleName}}
	if err := cl.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
		resultErr = errors.Join(resultErr, fmt.Errorf("[%s]: delete ClusterRole %q: %w", hookName, cr.Name, err))
	}

	return resultErr
}

func applyClusterRole(ctx context.Context, cl client.Client) error {
	existing := new(rbacv1.ClusterRole)
	err := cl.Get(ctx, client.ObjectKey{Name: consts.VRRProvisionerExecutorClusterRoleName}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("[%s]: get ClusterRole %q: %w", hookName, consts.VRRProvisionerExecutorClusterRoleName, err)
		}
		if createErr := cl.Create(ctx, desiredClusterRole()); createErr != nil {
			return fmt.Errorf("[%s]: create ClusterRole %q: %w", hookName, consts.VRRProvisionerExecutorClusterRoleName, createErr)
		}
		return nil
	}

	base := existing.DeepCopy()
	existing.Rules = desiredClusterRole().Rules
	existing.Labels = moduleLabels()
	if patchErr := cl.Patch(ctx, existing, client.MergeFrom(base)); patchErr != nil {
		return fmt.Errorf("[%s]: patch ClusterRole %q: %w", hookName, consts.VRRProvisionerExecutorClusterRoleName, patchErr)
	}
	return nil
}

func applyClusterRoleBinding(ctx context.Context, cl client.Client, namespace string) error {
	name := bindingName(namespace)
	existing := new(rbacv1.ClusterRoleBinding)
	err := cl.Get(ctx, client.ObjectKey{Name: name}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("[%s]: get ClusterRoleBinding %q: %w", hookName, name, err)
		}
		if createErr := cl.Create(ctx, desiredClusterRoleBinding(namespace)); createErr != nil {
			return fmt.Errorf("[%s]: create ClusterRoleBinding %q: %w", hookName, name, createErr)
		}
		return nil
	}

	// roleRef is immutable; only subjects and labels can drift.
	base := existing.DeepCopy()
	existing.Subjects = desiredSubjects(namespace)
	existing.Labels = moduleLabels()
	if patchErr := cl.Patch(ctx, existing, client.MergeFrom(base)); patchErr != nil {
		return fmt.Errorf("[%s]: patch ClusterRoleBinding %q: %w", hookName, name, patchErr)
	}
	return nil
}

// bindingName returns the per-namespace ClusterRoleBinding name.
func bindingName(namespace string) string {
	return consts.VRRProvisionerExecutorClusterRoleName + ":" + namespace
}

func desiredClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   consts.VRRProvisionerExecutorClusterRoleName,
			Labels: moduleLabels(),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"storage-foundation.deckhouse.io"},
				Resources: []string{"volumerestorerequests"},
				Verbs:     []string{"get", "list", "watch"},
			},
			{
				// The executor creates the target PVC after restoring the PV. Driver provisioner
				// roles grant PVC get/list/watch/update but not create, so create is the net-new verb.
				APIGroups: []string{""},
				Resources: []string{"persistentvolumeclaims"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
			},
		},
	}
}

func desiredClusterRoleBinding(namespace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   bindingName(namespace),
			Labels: moduleLabels(),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     consts.VRRProvisionerExecutorClusterRoleName,
		},
		Subjects: desiredSubjects(namespace),
	}
}

func desiredSubjects(namespace string) []rbacv1.Subject {
	return []rbacv1.Subject{{
		Kind:      "ServiceAccount",
		Name:      consts.CSIServiceAccountName,
		Namespace: namespace,
	}}
}

func moduleLabels() map[string]string {
	return map[string]string{
		"heritage": "deckhouse",
		"module":   consts.ModulePluralName,
	}
}
