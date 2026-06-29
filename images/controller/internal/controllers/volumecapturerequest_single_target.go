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

func setVolumeSnapshotDataRef(vcr *storagev1alpha1.VolumeCaptureRequest, target storagev1alpha1.VolumeCaptureTarget, vscName string) {
	binding := volumeSnapshotBinding(target, vscName)
	vcr.Status.DataRef = &binding
}

func setPersistentVolumeDataRef(vcr *storagev1alpha1.VolumeCaptureRequest, target storagev1alpha1.VolumeCaptureTarget, pvName string) {
	vcr.Status.DataRef = &storagev1alpha1.VolumeDataBinding{
		TargetUID: target.UID,
		Target:    target,
		Artifact: storagev1alpha1.VolumeDataArtifactRef{
			APIVersion: "v1",
			Kind:       "PersistentVolume",
			Name:       pvName,
		},
	}
}

func dataArtifactRef(status storagev1alpha1.VolumeCaptureRequestStatus) (storagev1alpha1.VolumeDataArtifactRef, bool) {
	if status.DataRef == nil {
		return storagev1alpha1.VolumeDataArtifactRef{}, false
	}
	return status.DataRef.Artifact, true
}
