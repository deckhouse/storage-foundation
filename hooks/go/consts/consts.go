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

	// AggregationAPICertCn is the Common Name and SAN base of the serving certificate of the module's
	// aggregated API server. It MUST equal the Service name in templates/controller/service.yaml: the
	// kube-apiserver aggregation layer dials <service>.<namespace>.svc and validates the presented
	// certificate against that DNS name, so a mismatch makes every aggregated request fail TLS
	// verification with the APIService reporting itself unavailable.
	AggregationAPICertCn string = "controller"

	// AggregationAPICertValuesKey is the internal values key under which the certificate hook publishes
	// the aggregated API server's crt/key/ca. It is the single name shared by the hook
	// (FullValuesPathPrefix) and the chart (templates/controller/{secret,apiservice}.yaml), which read
	// the serving pair and the caBundle from the same generated value so the APIService can never
	// advertise a CA that does not match the mounted certificate.
	AggregationAPICertValuesKey string = "aggregationApiCert"
)

const (
	// APIGroup is the DataExport/DataImport API group served by storage-foundation. This module
	// owns only the resources in this group: DataExport/DataImport served by another module under
	// a different API group belong to that module's controller and are never touched from here.
	APIGroup = "storage-foundation.deckhouse.io"
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
//
// This hook-side binding is unconditional. Driver modules rendered through lib-helm
// (helm_lib_csi_controller_rbac, deckhouse_lib_helm >= 1.72.13) also receive the same rules in
// their own provisioner ClusterRole, but only when the VolumeRestoreRequest API already exists at
// the time the driver module is rendered.
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
	Group   string
	Version string
	Kind    string
	// Namespaced is informational only. The on-delete cleanup lists ALL kinds cluster-wide:
	// the namespaced ones (VolumeSnapshot, DataExport, DataImport) live in arbitrary user
	// namespaces, so confining the list to the module namespace would miss them.
	Namespaced bool
}
