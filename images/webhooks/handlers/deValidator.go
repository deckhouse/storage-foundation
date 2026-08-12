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

package handlers

import (
	"context"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// volumeSnapshotContentKind / snapshotStorageGroup identify the cluster-scoped CSI VolumeSnapshotContent,
// which is forbidden as a DataExport target (a bare VSC has no tenant boundary -> cross-namespace
// exfiltration). Snapshot exports must reference a namespaced snapshot leaf instead. Both values are
// shared from api/v1alpha1 so the webhook and the controller's classifyTargetRef can never drift.
const (
	snapshotStorageGroup      = dev1alpha1.GroupSnapshotStorage
	volumeSnapshotContentKind = dev1alpha1.KindVolumeSnapshotContent
)

// DataExportValidateFunc returns the DataExport admission validator.
//
// C6 makes DataExport resource-agnostic: spec.targetRef is a GroupKind ({group, kind, name}, namespace
// implicit), not a fixed kind enum. There is therefore no compiled-in allowlist of kinds to validate
// against, and the controller (not the webhook) resolves the target and surfaces existence / readiness
// through DataExport conditions. The webhook keeps the two checks that must hold at admission time
// regardless of kind:
//   - structural: spec.targetRef.kind and spec.targetRef.name are set;
//   - security: a bare cluster-scoped VolumeSnapshotContent is rejected (cross-namespace exfiltration).
func DataExportValidateFunc() func(context.Context, *model.AdmissionReview, metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	return func(_ context.Context, ar *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
		// Only validate on CREATE. The DataExport CRD is marked x-kubernetes-immutable,
		// so spec changes on UPDATE are blocked by the API server itself.
		if ar.Operation != model.OperationCreate {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		dataExport, ok := obj.(*dev1alpha1.DataExport)
		if !ok {
			return &kwhvalidating.ValidatorResult{Valid: false, Message: "object is not a DataExport"}, nil
		}

		tr := dataExport.Spec.TargetRef
		if tr.Kind == "" || tr.Name == "" {
			return &kwhvalidating.ValidatorResult{Valid: false, Message: "spec.targetRef.kind or spec.targetRef.name is empty"}, nil
		}

		if tr.Group == snapshotStorageGroup && tr.Kind == volumeSnapshotContentKind {
			return &kwhvalidating.ValidatorResult{
				Valid:   false,
				Message: "spec.targetRef must not reference a bare cluster-scoped VolumeSnapshotContent; reference a namespaced snapshot resource instead",
			}, nil
		}

		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}
}
