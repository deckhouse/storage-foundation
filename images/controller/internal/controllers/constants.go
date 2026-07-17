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

// Mode constants for VolumeCaptureRequest
const (
	ModeSnapshot = "Snapshot"
	ModeDetach   = "Detach"
)

// Source kind constants for VolumeRestoreRequest
const (
	SourceKindVolumeSnapshotContent = "VolumeSnapshotContent"
	SourceKindPersistentVolume      = "PersistentVolume"
)

// Condition type and reason constants are defined in api/v1alpha1/conditions.go
// Import them from: github.com/deckhouse/storage-foundation/api/v1alpha1
// Use storagev1alpha1.ConditionTypeReady and storagev1alpha1.ConditionReason* instead of local constants

// Label key constants
const (
	LabelKeyManagedBy          = "app.kubernetes.io/managed-by"
	LabelKeyVCRUID             = "vcr-uid"
	LabelKeyVCRNamespace       = "vcr-namespace"
	LabelKeyVCRName            = "vcr-name"
	LabelKeyCreatedBy          = "storage-foundation.deckhouse.io/created-by"
	LabelKeyVCRNameFull        = "storage-foundation.deckhouse.io/vcr-name"
	LabelKeyVCRUIDFull         = "storage-foundation.deckhouse.io/vcr-uid"
	LabelKeySourcePVCName      = "storage-foundation.deckhouse.io/source-pvc-name"
	LabelKeySourcePVCNamespace = "storage-foundation.deckhouse.io/source-pvc-namespace"
	LabelKeyCSIVSCName         = "csi-vsc-name"
)

// Label value constants
const (
	LabelValueManagedBy    = "deckhouse-storage-foundation"
	LabelValueCreatedBy    = "VolumeCaptureRequest"
	LabelValueCreatedByVRR = "VolumeRestoreRequest"
)

// Name prefix constants
const (
	NamePrefixVCRCSIVS    = "vcr-csi-vs-"
	NamePrefixRetainer    = "ret-"
	NamePrefixRetainerVCR = "retainer-vcr-"
	NamePrefixRetainerPV  = "ret-pv-"
	NamePrefixRetainerPVC = "ret-pvc-"
	NamePrefixVRRTempVS   = "vrr-temp-vs-"
	NamePrefixTempVRR     = "temp-vrr-"
)

// API constants
const (
	APIGroupStorageDeckhouse  = "storage-foundation.deckhouse.io/v1alpha1"
	APIGroupSnapshotStorage   = "snapshot.storage.k8s.io"
	APIGroupDeckhouse         = "deckhouse.io/v1alpha1"
	KindObjectKeeper          = "ObjectKeeper"
	KindVolumeCaptureRequest  = "VolumeCaptureRequest"
	KindVolumeRestoreRequest  = "VolumeRestoreRequest"
	KindVolumeSnapshot        = "VolumeSnapshot"
	KindPersistentVolumeClaim = "PersistentVolumeClaim"
)

// Annotation key constants
const (
	AnnotationKeyCSIVSName = "storage-foundation.deckhouse.io/csi-vs-name"
	// AnnotationKeyReadyToStartSnapshot is set on VCR to signal that snapshot-controller should create VSC
	// This annotation contains VCR UID and is used by snapshot-controller to identify VCR requests
	AnnotationKeyReadyToStartSnapshot = "volumesnapshot.deckhouse.io/vcr"
)
