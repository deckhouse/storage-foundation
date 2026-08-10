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
	"log/slog"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/sds-common-lib/slogh"
)

// VolumeSnapshotValidateFunc refuses a VolumeSnapshot whose StorageClass wiring cannot produce a working
// snapshot, with the reason stated in the denial message.
//
// The verdict itself is resolveVolumeSnapshotClass's, shared with the mutator, but only a validating
// webhook can deliver it as a denial: kubewebhook answers a mutator error with HTTP 500, which the API
// server reports as "Internal error occurred: ... an error on the server (...)" — the operator sees a
// broken cluster instead of a missing annotation, and a 500 also invites clients to retry a request that
// can never succeed. Here the answer is HTTP 200 with code 400, which kubectl prints as
// "admission webhook ... denied the request: <message>".
func VolumeSnapshotValidateFunc() validateFunc {
	return func(ctx context.Context, ar *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
		log := slog.New(slogh.NewHandler(slogh.Config{}))

		// Only CREATE is registered; spec.volumeSnapshotClassName is immutable afterwards.
		if ar.Operation != model.OperationCreate {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		snapshot, ok := obj.(*snapshotv1.VolumeSnapshot)
		if !ok {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		sc, found, err := storageClassOfPVCSource(ctx, log, snapshot)
		if err != nil {
			return nil, err
		}
		if !found {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		return volumeSnapshotClassVerdict(log, sc, snapshot), nil
	}
}

func volumeSnapshotClassVerdict(log *slog.Logger, sc *storagev1.StorageClass, snapshot *snapshotv1.VolumeSnapshot) *kwhvalidating.ValidatorResult {
	if _, _, err := resolveVolumeSnapshotClass(sc, snapshot.Spec.VolumeSnapshotClassName); err != nil {
		log.Error("VolumeSnapshotValidate: denying VolumeSnapshot", "snapshot", snapshot.Name, "storageClass", sc.Name, "reason", err)
		return &kwhvalidating.ValidatorResult{Valid: false, Message: err.Error()}
	}
	return &kwhvalidating.ValidatorResult{Valid: true}
}
