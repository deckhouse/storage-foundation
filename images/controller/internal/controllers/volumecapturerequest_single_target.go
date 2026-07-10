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

// requireCaptureTarget returns the single spec.target, erroring when it is absent.
func requireCaptureTarget(spec storagev1alpha1.VolumeCaptureRequestSpec) (storagev1alpha1.VolumeCaptureTarget, error) {
	if spec.Target == nil {
		return storagev1alpha1.VolumeCaptureTarget{}, fmt.Errorf("spec.target is required")
	}
	return *spec.Target, nil
}

func setVolumeSnapshotDataRef(vcr *storagev1alpha1.VolumeCaptureRequest, target storagev1alpha1.VolumeCaptureTarget, vscName, vscUID string) {
	binding := volumeSnapshotBinding(target, vscName, vscUID)
	vcr.Status.Data = &binding
}

func setPersistentVolumeDataRef(vcr *storagev1alpha1.VolumeCaptureRequest, _ storagev1alpha1.VolumeCaptureTarget, pvName, pvUID string) {
	vcr.Status.Data = &storagev1alpha1.VolumeDataBinding{
		Artifact: storagev1alpha1.VolumeDataArtifactRef{
			APIVersion: "v1",
			Kind:       "PersistentVolume",
			Name:       pvName,
			// UID is best-effort: empty when the PV object is not available.
			UID: pvUID,
		},
	}
}

func dataArtifactRef(status storagev1alpha1.VolumeCaptureRequestStatus) (storagev1alpha1.VolumeDataArtifactRef, bool) {
	if status.Data == nil {
		return storagev1alpha1.VolumeDataArtifactRef{}, false
	}
	return status.Data.Artifact, true
}
