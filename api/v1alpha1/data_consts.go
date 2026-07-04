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

package v1alpha1

// List of constants related to CRD schema
// TODO: some of DataExport constants are not extracted here yet
const (
	LabelApplicationKey = "app"

	LabelDataExportValue = "data-exporter"

	LabelDataImportValue = "data-importer"

	AnnotationStorageManagerNamespaceKey = "storage.deckhouse.io/storage-manager-namespace"
	AnnotationStorageManagerNameKey      = "storage.deckhouse.io/storage-manager-name"
	LabelStorageManagerDeploymentNameKey = "storage.deckhouse.io/storage-manager-deployment-name"
	StorageManagerFinalizerName          = "storage.deckhouse.io/storage-manager-controller"

	AuthTypeBearer = "Bearer"
	AuthTypeBasic  = "Basic"
	AuthTypeCert   = "Cert"

	KindPVC            = "PersistentVolumeClaim"
	KindVolumeSnapshot = "VolumeSnapshot"
	KindVirtualDisk    = "VirtualDisk"
	// KindVolumeSnapshotContent is the cluster-scoped CSI artifact kind. It is the security boundary
	// for DataExport targetRef: a bare VolumeSnapshotContent has no tenant namespace and is rejected
	// by both the admission webhook and the controller (shared from here so the two never drift).
	KindVolumeSnapshotContent = "VolumeSnapshotContent"

	// GroupSnapshotStorage / GroupVirtualization are the API groups used when matching DataExport
	// targetRef GroupKinds (shared by the controller's classifyTargetRef and the admission webhook).
	GroupSnapshotStorage = "snapshot.storage.k8s.io"
	GroupVirtualization  = "virtualization.deckhouse.io"

	// AnnotationUserPVCNamespaceKey is a PV annotation that stores the original user PVC namespace.
	// Used to restore the PV binding back to user PVC after export cleanup.
	AnnotationUserPVCNamespaceKey = "storage.deckhouse.io/original-pvc-namespace"
	// AnnotationUserPVCNameKey is a PV annotation that stores the original user PVC name.
	// Used together with AnnotationUserPVCNamespaceKey to restore PV binding after export.
	AnnotationUserPVCNameKey = "storage.deckhouse.io/original-pvc-name"

	// AnnotationPVTargetKindShortKey is a PV annotation that stores the target kind short name (pvc/vd/vs/vdsnapshot).
	// Used to reconstruct deployment and exportPVC names during orphan cleanup when DataExport is already deleted.
	AnnotationPVTargetKindShortKey = "storage.deckhouse.io/target-ref"
	// AnnotationPVHashSuffixKey is a PV annotation that stores the hash suffix from generated names.
	// Used together with AnnotationPVTargetKindShortKey to reconstruct resource names during orphan cleanup.
	AnnotationPVHashSuffixKey = "storage.deckhouse.io/generated-name-suffix"

	// AnnotationOriginalReclaimPolicyKey stores the PV's original reclaim policy before
	// we change it to Retain during export. This protects data if exportPVC is deleted
	// accidentally. The original policy is restored during cleanup.
	AnnotationOriginalReclaimPolicyKey = "storage.deckhouse.io/original-reclaim-policy"

	// LabelPVDataExporter is added to PVs during export to enable efficient
	// discovery of orphaned PVs via MatchingLabels in removeOrphanResources.
	LabelPVDataExporter = "storage-foundation.deckhouse.io/data-exporter"

	// LabelPVDataExporterInconsistent is set on PV when data-export annotations are
	// corrupted or PV is in an unexpected state. Administrators should investigate.
	// Values: "warning" or "error".
	LabelPVDataExporterInconsistent = "storage-foundation.deckhouse.io/data-exporter-inconsistent"

	KindPVCShort                 = "pvc"
	KindVolumeSnapshotShort      = "vs"
	KindVirtualDiskShort         = "vd"
	KindVirtualDiskSnapshotShort = "vdsnapshot"
	// KindSnapshotShort is the generic short kind for ALL snapshot-backed export targets (C6): any
	// registered snapshot CR (generic VolumeSnapshot, VirtualDiskSnapshot, domain snapshot, ...) is
	// exported through the same resource-agnostic VRR path keyed off the leaf's SnapshotContent.dataRef,
	// so they share one short for deterministic resource naming / orphan recovery. The live PVC/VirtualDisk
	// direct-export paths keep their own shorts (pvc/vd).
	KindSnapshotShort = "snap"

	VirtualDiskCRDName         = "virtualdisks.virtualization.deckhouse.io"
	VirtualDiskSnapshotCRDName = "virtualdisksnapshots.virtualization.deckhouse.io"
)

// Public ingress targets
const (
	PublicIngressKubernetesAPI   = "KubernetesAPI"
	PublicIngressConsoleFrontend = "ConsoleFrontend"
)
