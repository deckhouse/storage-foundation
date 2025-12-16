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

// Error reason constants
const (
	ErrorReasonInternalError          = "InternalError"
	ErrorReasonNotFound               = "NotFound"
	ErrorReasonRBACDenied             = "RBACDenied"
	ErrorReasonInvalidMode            = "InvalidMode"
	ErrorReasonInvalidSource          = "InvalidSource"
	ErrorReasonPVBound                = "PVBound"
	ErrorReasonIncompatible           = "Incompatible"           // For WFFC and cross-SC restore
	ErrorReasonSnapshotCreationFailed = "SnapshotCreationFailed" // For CSI snapshot creation failures
)

// Condition type constants
const (
	ConditionTypeReady = "Ready"
)

// Condition reason constants
const (
	ConditionReasonCompleted    = "Completed"
	ConditionReasonInvalidMode  = "InvalidMode"
	ConditionReasonIncompatible = "Incompatible" // For WFFC and cross-SC restore
	ConditionReasonInvalidTTL   = "InvalidTTL"   // For invalid TTL annotation format
)

// Label key constants
const (
	LabelKeyManagedBy          = "app.kubernetes.io/managed-by"
	LabelKeyVCRUID             = "vcr-uid"
	LabelKeyVCRNamespace       = "vcr-namespace"
	LabelKeyVCRName            = "vcr-name"
	LabelKeyCreatedBy          = "storage.deckhouse.io/created-by"
	LabelKeyVCRNameFull        = "storage.deckhouse.io/vcr-name"
	LabelKeyVCRUIDFull         = "storage.deckhouse.io/vcr-uid"
	LabelKeySourcePVCName      = "storage.deckhouse.io/source-pvc-name"
	LabelKeySourcePVCNamespace = "storage.deckhouse.io/source-pvc-namespace"
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
	NamePrefixVCRCSIVS   = "vcr-csi-vs-"
	NamePrefixRetainer   = "ret-"
	NamePrefixRetainerPV = "ret-pv-"
	NamePrefixVRRTempVS  = "vrr-temp-vs-"
	NamePrefixTempVRR    = "temp-vrr-"
)

// API constants
const (
	APIGroupStorageDeckhouse = "storage.deckhouse.io/v1alpha1"
	APIGroupSnapshotStorage  = "snapshot.storage.k8s.io"
	APIGroupDeckhouse        = "deckhouse.io/v1alpha1"
	KindObjectKeeper         = "ObjectKeeper"
	KindVolumeCaptureRequest = "VolumeCaptureRequest"
	KindVolumeSnapshot       = "VolumeSnapshot"
)

// Annotation key constants
const (
	AnnotationKeyCSIVSName = "storage.deckhouse.io/csi-vs-name"
	AnnotationKeyTTL       = "storage.deckhouse.io/ttl" // TTL annotation for automatic deletion
	// AnnotationKeyReadyToStartSnapshot is set on VCR to signal that snapshot-controller should create VSC
	// This annotation contains VCR UID and is used by snapshot-controller to identify VCR requests
	AnnotationKeyReadyToStartSnapshot = "volumesnapshot.deckhouse.io/vcr"
)

// TTL constants
const (
	DefaultVolumeRequestTTL = "10m" // Default TTL for VolumeCaptureRequest and VolumeRestoreRequest
	// DefaultRetentionSnapshotTTL and DefaultRetentionDetachTTL are defined in config package
	// They are used as fallback if config is not available
)

// Service namespace constant
const (
	ServiceNamespace = "d8-storage-foundation" // Default service namespace for temporary VolumeSnapshot objects
)
