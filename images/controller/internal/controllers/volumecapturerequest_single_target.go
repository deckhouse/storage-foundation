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
	"fmt"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

const prf2BulkTargetsMessage = "bulk VCR controller not implemented (PR-F-2)"

// singleVolumeCaptureTarget is a PR-F-1 compile shim: existing controller logic still handles one PVC.
// PR-F-2 replaces this with a per-target loop over spec.targets[].
func singleVolumeCaptureTarget(spec storagev1alpha1.VolumeCaptureRequestSpec) (storagev1alpha1.VolumeCaptureTarget, error) {
	switch len(spec.Targets) {
	case 0:
		return storagev1alpha1.VolumeCaptureTarget{}, fmt.Errorf("spec.targets is required")
	case 1:
		return spec.Targets[0], nil
	default:
		return storagev1alpha1.VolumeCaptureTarget{}, fmt.Errorf("%s: expected one target, got %d", prf2BulkTargetsMessage, len(spec.Targets))
	}
}

func setVolumeSnapshotDataRef(vcr *storagev1alpha1.VolumeCaptureRequest, target storagev1alpha1.VolumeCaptureTarget, vscName string) {
	vcr.Status.DataRefs = []storagev1alpha1.VolumeDataBinding{{
		TargetUID: target.UID,
		Target:    target,
		Artifact: storagev1alpha1.VolumeDataArtifactRef{
			APIVersion: "snapshot.storage.k8s.io/v1",
			Kind:       "VolumeSnapshotContent",
			Name:       vscName,
		},
	}}
}

func setPersistentVolumeDataRef(vcr *storagev1alpha1.VolumeCaptureRequest, target storagev1alpha1.VolumeCaptureTarget, pvName string) {
	vcr.Status.DataRefs = []storagev1alpha1.VolumeDataBinding{{
		TargetUID: target.UID,
		Target:    target,
		Artifact: storagev1alpha1.VolumeDataArtifactRef{
			APIVersion: "v1",
			Kind:       "PersistentVolume",
			Name:       pvName,
		},
	}}
}

func firstDataArtifactRef(status storagev1alpha1.VolumeCaptureRequestStatus) (storagev1alpha1.VolumeDataArtifactRef, bool) {
	if len(status.DataRefs) == 0 {
		return storagev1alpha1.VolumeDataArtifactRef{}, false
	}
	return status.DataRefs[0].Artifact, true
}
