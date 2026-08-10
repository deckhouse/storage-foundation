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

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// vcrExpectedVSCNameIndex indexes each Snapshot-mode VolumeCaptureRequest by the name of the
// VolumeSnapshotContent it is going to create. The name is fully determined by the request itself
// (VCR UID + target UID), so a content event can be resolved back to its owner by an indexed
// lookup instead of listing every request in the cluster.
const vcrExpectedVSCNameIndex = "spec.expectedVolumeSnapshotContentName"

// indexVCRByExpectedVSCName is the index function for vcrExpectedVSCNameIndex. Requests that
// cannot own a content — a different mode, no target, or an object that has not been assigned a
// UID yet — are simply not indexed.
func indexVCRByExpectedVSCName(obj client.Object) []string {
	vcr, ok := obj.(*storagev1alpha1.VolumeCaptureRequest)
	if !ok {
		return nil
	}
	if vcr.Spec.Mode != ModeSnapshot {
		return nil
	}
	if vcr.UID == "" || vcr.Spec.Target == nil || vcr.Spec.Target.UID == "" {
		return nil
	}
	return []string{snapshotVSCName(vcr.UID, vcr.Spec.Target.UID)}
}

// mapVSCToVCR resolves a VolumeSnapshotContent event to the request that owns it.
//
// The lookup is indexed, so its cost does not grow with the number of requests in the cluster, and
// contents produced by anything other than a capture request (restores, other modules, manually
// created contents) resolve to nothing and cost one index probe.
func (r *VolumeCaptureRequestController) mapVSCToVCR(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &storagev1alpha1.VolumeCaptureRequestList{}
	if err := r.List(ctx, list, client.MatchingFields{vcrExpectedVSCNameIndex: obj.GetName()}); err != nil {
		// Losing an event only costs latency: the periodic requeue still makes progress.
		log.FromContext(ctx).Error(err, "failed to map VolumeSnapshotContent to VolumeCaptureRequest",
			"volumeSnapshotContent", obj.GetName())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return requests
}
