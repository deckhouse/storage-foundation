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

// detachVolumeCaptureTarget validates Detach mode still uses exactly one target (bulk Detach out of scope).
func detachVolumeCaptureTarget(spec storagev1alpha1.VolumeCaptureRequestSpec) (storagev1alpha1.VolumeCaptureTarget, error) {
	switch len(spec.Targets) {
	case 0:
		return storagev1alpha1.VolumeCaptureTarget{}, fmt.Errorf("spec.targets is required")
	case 1:
		return spec.Targets[0], nil
	default:
		return storagev1alpha1.VolumeCaptureTarget{}, fmt.Errorf("Detach mode supports exactly one target, got %d", len(spec.Targets))
	}
}

func setVolumeSnapshotDataRef(vcr *storagev1alpha1.VolumeCaptureRequest, target storagev1alpha1.VolumeCaptureTarget, vscName string) {
	binding := volumeSnapshotBinding(target, vscName)
	vcr.Status.DataRefs = upsertVolumeDataBinding(vcr.Status.DataRefs, binding)
}

func setPersistentVolumeDataRef(vcr *storagev1alpha1.VolumeCaptureRequest, target storagev1alpha1.VolumeCaptureTarget, pvName string) {
	vcr.Status.DataRefs = upsertVolumeDataBinding(vcr.Status.DataRefs, storagev1alpha1.VolumeDataBinding{
		TargetUID: target.UID,
		Target:    target,
		Artifact: storagev1alpha1.VolumeDataArtifactRef{
			APIVersion: "v1",
			Kind:       "PersistentVolume",
			Name:       pvName,
		},
	})
}

func firstDataArtifactRef(status storagev1alpha1.VolumeCaptureRequestStatus) (storagev1alpha1.VolumeDataArtifactRef, bool) {
	if len(status.DataRefs) == 0 {
		return storagev1alpha1.VolumeDataArtifactRef{}, false
	}
	return status.DataRefs[0].Artifact, true
}
