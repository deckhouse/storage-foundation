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

// Package hooks_common migrates DataExport/DataImport resources from the legacy
// storage.deckhouse.io API group to the unified storage-foundation.deckhouse.io group
// served by storage-foundation, and removes the legacy CRDs.
//
// This is a mirror of the storage-volume-data-manager (svdm) v0.2.0 hook of the same name.
// Both run OnBeforeHelm, are idempotent, and gate on the presence of the legacy CRDs, so
// whichever module converges first performs the migration while the other no-ops. The svdm
// copy covers the svdm-standalone path (storage-foundation disabled); this copy covers the
// path where storage-foundation is enabled without svdm ever converging on v0.2.0 (e.g. svdm
// disabled at <=0.1.24 then storage-foundation enabled). Deckhouse does not delete CRDs on
// module disable, so without this hook the legacy CRDs and orphaned objects would live forever
// and legacy PVC finalizers could hold PVCs in Terminating.
//
// The contractual core — the exportKindToGroup table, the two spec transforms in mapLegacySpec,
// and isActiveLegacyResource — MUST stay in sync with the svdm copy; hack/check-migration-hook-parity.sh
// guards against drift. Both 025 hooks are transitional/expiring: delete them once every cluster
// has passed the legacy epoch.
//
// The hook runs on every beforeHelm converge and is idempotent: when the legacy CRDs are
// absent (fresh install or migration already done) it is a no-op. When they are present:
//   - unfinished, non-failed resources are re-created under the new group with the spec
//     mapped to the unified contract (statuses are not carried over — the controller
//     re-reconciles the new objects from scratch);
//   - finalizers are stripped from ALL legacy resources (their controller is gone, so
//     nothing would ever remove them and CRD deletion would hang);
//   - the legacy CRDs are deleted, cascading deletion of the remaining legacy objects.
package hooks_common

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/module-sdk/pkg"
	"github.com/deckhouse/module-sdk/pkg/registry"
	"github.com/deckhouse/sds-common-lib/kubeclient"
	"github.com/deckhouse/storage-foundation/hooks/go/consts"
)

const migrateHookName = "migrate-legacy-crds"

var _ = registry.RegisterFunc(configMigrateLegacyCRDs, handlerMigrateLegacyCRDs)

var configMigrateLegacyCRDs = &pkg.HookConfig{
	OnBeforeHelm: &pkg.OrderedConfig{Order: 5},
}

type legacyCRD struct {
	crdName string
	kind    string
}

var legacyCRDs = []legacyCRD{
	{crdName: consts.LegacyDataExportCRDName, kind: "DataExport"},
	{crdName: consts.LegacyDataImportCRDName, kind: "DataImport"},
}

// exportKindToGroup maps a legacy DataExport targetRef.kind to the API group required by the
// unified targetRef {group, kind, name} contract.
//
// PARITY: must match the svdm 025 copy — extend both when a new DataExport target kind is added.
var exportKindToGroup = map[string]string{
	"PersistentVolumeClaim": "",
	"VolumeSnapshot":        "snapshot.storage.k8s.io",
	"VirtualDisk":           "virtualization.deckhouse.io",
	"VirtualDiskSnapshot":   "virtualization.deckhouse.io",
}

// handlerMigrateLegacyCRDs is the thin OnBeforeHelm wrapper: it builds the kube client and
// delegates to migrate, which holds the testable migration logic.
func handlerMigrateLegacyCRDs(ctx context.Context, input *pkg.HookInput) error {
	cl, err := kubeclient.New(clientgoscheme.AddToScheme, extv1.AddToScheme)
	if err != nil {
		return fmt.Errorf("[%s]: failed to initialize kube client: %w", migrateHookName, err)
	}
	return migrate(ctx, cl, input.Logger)
}

func migrate(ctx context.Context, cl client.Client, logger pkg.Logger) error {
	// The legacy CRDs double as the migration-epoch marker: while at least one is present,
	// pre-unification leftovers (including legacy PVC finalizers) may exist. They are deleted
	// only after every migration step succeeded, so a failed converge retries everything.
	legacyEpoch := false
	presentCRDs := map[string]*extv1.CustomResourceDefinition{}
	var resultErr error
	for _, legacy := range legacyCRDs {
		crd := &extv1.CustomResourceDefinition{}
		if err := cl.Get(ctx, client.ObjectKey{Name: legacy.crdName}, crd); err != nil {
			if apierrors.IsNotFound(err) {
				continue // nothing to migrate — fresh install or already migrated
			}
			resultErr = errors.Join(resultErr, fmt.Errorf("[%s]: failed to get CRD %s: %w", migrateHookName, legacy.crdName, err))
			continue
		}
		legacyEpoch = true
		presentCRDs[legacy.crdName] = crd
	}
	if !legacyEpoch {
		return resultErr
	}

	// The pre-unification controller protected PVCs of in-flight exports/imports with the legacy
	// finalizer; the unified controller manages only the new name, so strip the legacy one
	// here — otherwise such PVCs hang in Terminating forever once deleted.
	sweepErr := stripLegacyPVCFinalizers(ctx, cl, logger)
	resultErr = errors.Join(resultErr, sweepErr)

	for _, legacy := range legacyCRDs {
		crd, present := presentCRDs[legacy.crdName]
		if !present {
			continue
		}

		logger.Info(fmt.Sprintf("[%s]: legacy CRD %s found, migrating resources to group %s", migrateHookName, legacy.crdName, consts.APIGroup))

		if err := migrateLegacyResources(ctx, cl, logger, legacy); err != nil || sweepErr != nil {
			// Do not delete the CRD while any migration step failed: the hook will retry
			// on the next converge with the legacy epoch marker still in place.
			resultErr = errors.Join(resultErr, err)
			continue
		}

		logger.Info(fmt.Sprintf("[%s]: deleting legacy CRD %s", migrateHookName, legacy.crdName))
		if err := cl.Delete(ctx, crd); err != nil && !apierrors.IsNotFound(err) {
			resultErr = errors.Join(resultErr, fmt.Errorf("[%s]: failed to delete CRD %s: %w", migrateHookName, legacy.crdName, err))
		}
	}
	return resultErr
}

// stripLegacyPVCFinalizers removes the pre-unification finalizer from every PVC carrying it,
// keeping all other finalizers (including the new-name one the controller manages) intact.
func stripLegacyPVCFinalizers(ctx context.Context, cl client.Client, logger pkg.Logger) error {
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := cl.List(ctx, pvcList); err != nil {
		return fmt.Errorf("[%s]: failed to list PVCs: %w", migrateHookName, err)
	}

	var resultErr error
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		kept := make([]string, 0, len(pvc.Finalizers))
		for _, f := range pvc.Finalizers {
			if f != consts.LegacyStorageManagerFinalizerName {
				kept = append(kept, f)
			}
		}
		if len(kept) == len(pvc.Finalizers) {
			continue
		}
		// Optimistic lock: the merge patch replaces the whole finalizers list, so without it a
		// finalizer added by another controller between our List and this Patch (e.g.
		// snapshot.storage.kubernetes.io/pvc-as-source-protection during an in-flight snapshot)
		// would be silently removed. A Conflict instead fails the sweep and retries next converge.
		patch := client.MergeFromWithOptions(pvc.DeepCopy(), client.MergeFromWithOptimisticLock{})
		pvc.SetFinalizers(kept)
		err := cl.Patch(ctx, pvc, patch)
		if apierrors.IsNotFound(err) {
			// Concurrently deleted (e.g. the svdm 025 hook stripped it and the PVC then finished
			// terminating): the goal — no legacy finalizer left — is already met, so this is not
			// an error. The two hooks race by design and must both treat missing objects as done.
			continue
		}
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("[%s]: failed to strip legacy finalizer from PVC %s/%s: %w", migrateHookName, pvc.Namespace, pvc.Name, err))
			continue
		}
		logger.Info(fmt.Sprintf("[%s]: stripped legacy finalizer from PVC %s/%s", migrateHookName, pvc.Namespace, pvc.Name))
	}
	return resultErr
}

func migrateLegacyResources(ctx context.Context, cl client.Client, logger pkg.Logger, legacy legacyCRD) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: consts.LegacyAPIGroup, Version: "v1alpha1", Kind: legacy.kind + "List"})
	if err := cl.List(ctx, list); err != nil {
		// A concurrent svdm 025 run may have deleted this CRD between migrate's up-front presence
		// check and this List (the two hooks race by design). If the CRD is now gone, the other run
		// owns the migration — treat it as done rather than failing the converge and forcing a retry.
		crd := &extv1.CustomResourceDefinition{}
		if getErr := cl.Get(ctx, client.ObjectKey{Name: legacy.crdName}, crd); apierrors.IsNotFound(getErr) {
			return nil
		}
		return fmt.Errorf("[%s]: failed to list legacy %s: %w", migrateHookName, legacy.kind, err)
	}

	var resultErr error
	for i := range list.Items {
		old := &list.Items[i]

		if isActiveLegacyResource(old) {
			if err := createUnifiedCounterpart(ctx, cl, logger, legacy.kind, old); err != nil {
				resultErr = errors.Join(resultErr, err)
				continue // keep the legacy object (and its CRD) until migration succeeds
			}
		} else {
			logger.Info(fmt.Sprintf("[%s]: skipping finished/failed legacy %s %s/%s", migrateHookName, legacy.kind, old.GetNamespace(), old.GetName()))
		}

		// The legacy controller is gone: strip finalizers so the CRD deletion above can
		// cascade-delete the leftovers instead of hanging in Terminating forever.
		// A NotFound here means a concurrent svdm 025 run (or its CRD-deletion cascade) already
		// removed the object — the desired end state, so IgnoreNotFound treats it as success
		// rather than failing the converge and forcing a needless retry.
		// Deliberately NO optimistic lock (unlike the PVC sweep): the object is being force-cleaned
		// right before its CRD is deleted, and a still-running legacy controller re-adding its
		// finalizer must not livelock the migration with endless Conflicts.
		if len(old.GetFinalizers()) > 0 {
			patch := client.MergeFrom(old.DeepCopy())
			old.SetFinalizers(nil)
			if err := cl.Patch(ctx, old, patch); client.IgnoreNotFound(err) != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("[%s]: failed to strip finalizers from legacy %s %s/%s: %w", migrateHookName, legacy.kind, old.GetNamespace(), old.GetName(), err))
			}
		}
	}
	return resultErr
}

// isActiveLegacyResource reports whether a legacy resource is still in progress: neither
// expired, nor completed, nor failed. Only such resources are worth re-creating under the
// new group — finished sessions die together with the legacy CRD.
//
// PARITY: must match the svdm 025 copy — the failedReasons set is part of the shared contract.
func isActiveLegacyResource(obj *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return true // no conditions yet — freshly created, migrate it
	}

	failedReasons := map[string]bool{
		"Expired":          true,
		"ValidationFailed": true,
		"TargetNotFound":   true,
		"TargetFailed":     true,
		"PVConflict":       true,
		"DeploymentFailed": true,
		"CleanupFailed":    true,
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		status, _ := cond["status"].(string)
		reason, _ := cond["reason"].(string)

		if condType == "Completed" && status == string(metav1.ConditionTrue) {
			return false
		}
		if condType == "Expired" && status == string(metav1.ConditionTrue) {
			return false
		}
		if failedReasons[reason] {
			return false
		}
	}
	return true
}

// mapLegacySpec converts a legacy DataExport/DataImport spec to the unified contract, mutating
// and returning the passed spec map. It is a pure function (no cluster access) so it can be
// table-tested directly.
//
//   - DataExport: {targetRef: {kind, name}} -> {targetRef: {group, kind, name}} with group looked
//     up in exportKindToGroup (an unknown kind is an error; PersistentVolumeClaim maps to the core
//     group "", in which case no group field is written).
//   - DataImport: {targetRef: {kind, pvcTemplate}} -> {mode: CreatePVC, pvcTemplate} (targetRef is
//     dropped; a missing targetRef.pvcTemplate is an error).
//
// PARITY: the two transforms below are the shared contract — keep them identical to the svdm copy.
func mapLegacySpec(kind string, spec map[string]any) (map[string]any, error) {
	switch kind {
	case "DataExport":
		// {targetRef: {kind, name}} -> {targetRef: {group, kind, name}}
		targetKind, _, _ := unstructured.NestedString(spec, "targetRef", "kind")
		group, known := exportKindToGroup[targetKind]
		if !known {
			return nil, fmt.Errorf("unsupported targetRef.kind %q", targetKind)
		}
		if group != "" {
			if err := unstructured.SetNestedField(spec, group, "targetRef", "group"); err != nil {
				return nil, fmt.Errorf("failed to set targetRef.group: %w", err)
			}
		}
	case "DataImport":
		// {targetRef: {kind, pvcTemplate}} -> {mode: CreatePVC, pvcTemplate}
		pvcTemplate, found, _ := unstructured.NestedMap(spec, "targetRef", "pvcTemplate")
		if !found {
			return nil, fmt.Errorf("no targetRef.pvcTemplate")
		}
		unstructured.RemoveNestedField(spec, "targetRef")
		if err := unstructured.SetNestedField(spec, "CreatePVC", "mode"); err != nil {
			return nil, fmt.Errorf("failed to set mode: %w", err)
		}
		if err := unstructured.SetNestedMap(spec, pvcTemplate, "pvcTemplate"); err != nil {
			return nil, fmt.Errorf("failed to set pvcTemplate: %w", err)
		}
	default:
		return nil, fmt.Errorf("unexpected kind %s", kind)
	}
	return spec, nil
}

// createUnifiedCounterpart re-creates a legacy resource under the unified group with the
// spec mapped to the new contract. The status is intentionally dropped: the controller
// reconciles the new object from scratch (same name/namespace, so the generated runtime
// resource names stay stable).
func createUnifiedCounterpart(ctx context.Context, cl client.Client, logger pkg.Logger, kind string, old *unstructured.Unstructured) error {
	spec, found, err := unstructured.NestedMap(old.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("[%s]: legacy %s %s/%s has no readable spec: %v", migrateHookName, kind, old.GetNamespace(), old.GetName(), err)
	}

	spec, err = mapLegacySpec(kind, spec)
	if err != nil {
		return fmt.Errorf("[%s]: legacy %s %s/%s: %w", migrateHookName, kind, old.GetNamespace(), old.GetName(), err)
	}

	migrated := &unstructured.Unstructured{}
	migrated.SetGroupVersionKind(schema.GroupVersionKind{Group: consts.APIGroup, Version: "v1alpha1", Kind: kind})
	migrated.SetNamespace(old.GetNamespace())
	migrated.SetName(old.GetName())
	migrated.SetLabels(old.GetLabels())
	migrated.SetAnnotations(old.GetAnnotations())
	if err := unstructured.SetNestedMap(migrated.Object, spec, "spec"); err != nil {
		return fmt.Errorf("[%s]: failed to build migrated %s %s/%s: %w", migrateHookName, kind, old.GetNamespace(), old.GetName(), err)
	}

	if err := cl.Create(ctx, migrated); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // previous (partially failed) run already created it
		}
		return fmt.Errorf("[%s]: failed to create migrated %s %s/%s: %w", migrateHookName, kind, old.GetNamespace(), old.GetName(), err)
	}

	logger.Info(fmt.Sprintf("[%s]: migrated %s %s/%s to group %s", migrateHookName, kind, old.GetNamespace(), old.GetName(), consts.APIGroup))
	return nil
}
