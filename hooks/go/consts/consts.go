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

package consts

const (
	ModuleName              string = "storageFoundation"
	ModuleNamespace         string = "d8-storage-foundation"
	ModulePluralName        string = "storage-foundation"
	ValidatingWebhookCertCn string = "snapshot-validation-webhook"
	WebhookCertCn           string = "webhooks"
)

const (
	// APIGroup is the unified DataExport/DataImport API group served by storage-foundation
	// (shared with the storage-volume-data-manager module since its v0.2.0).
	APIGroup = "storage-foundation.deckhouse.io"
	// LegacyAPIGroup is the pre-unification API group. The 025-migrate-legacy-crds hook moves
	// live DataExport/DataImport resources from it to APIGroup and deletes the legacy CRDs.
	LegacyAPIGroup = "storage.deckhouse.io"

	// LegacyStorageManagerFinalizerName is the pre-unification finalizer the old controller put
	// on PVCs involved in an export/import. The unified controller only manages resources under
	// APIGroup, so the migration hook strips the legacy finalizer (otherwise such PVCs would hang
	// in Terminating forever once deleted).
	LegacyStorageManagerFinalizerName = LegacyAPIGroup + "/storage-manager-controller"

	// LegacyDataExportCRDName and LegacyDataImportCRDName are the names of the pre-unification
	// CRDs the migration hook deletes after moving live resources to APIGroup.
	LegacyDataExportCRDName = "dataexports." + LegacyAPIGroup
	LegacyDataImportCRDName = "dataimports." + LegacyAPIGroup
)

const (
	// VRRProvisionerExecutorClusterRoleName is the cluster-wide ClusterRole granting the patched
	// csi-provisioner VRR executor read access to VolumeRestoreRequest across all namespaces.
	VRRProvisionerExecutorClusterRoleName string = "d8:storage-foundation:vrr-provisioner-executor"
	// CSIServiceAccountName is the ServiceAccount under which backend CSI driver controller Pods
	// (and the patched csi-provisioner sidecar) run in their module namespace.
	CSIServiceAccountName string = "csi"
)

// VRRExecutorNamespaces is the hardcoded list of backend CSI module namespaces whose `csi`
// ServiceAccount runs the patched csi-provisioner VRR executor and therefore needs cluster-wide
// get/list/watch on volumerestorerequests. Binding to a namespace/ServiceAccount that does not
// exist yet is harmless (it takes effect once the module is enabled). For now only sds-local-volume.
var VRRExecutorNamespaces = []string{
	"d8-sds-local-volume",
}

var AllowedProvisioners = []string{}

var WebhookConfigurationsToDelete = []string{
	// ValidatingWebhookConfiguration for DataExport (templates/webhooks/dataexport-validation.yaml).
	// Removed on module deletion so the API server stops calling the webhooks service once it is gone.
	ModuleNamespace + "-dataexport-validation",
}

var CRGVKsForFinalizerRemoval = []CRGVK{
	{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot", Namespaced: true},
	{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent", Namespaced: false},
	{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass", Namespaced: false},
	{Group: APIGroup, Version: "v1alpha1", Kind: "DataExport", Namespaced: true},
	{Group: APIGroup, Version: "v1alpha1", Kind: "DataImport", Namespaced: true},
}

type CRGVK struct {
	Group      string
	Version    string
	Kind       string
	Namespaced bool
}
