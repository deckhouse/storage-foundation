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

import (
	"crypto/x509"

	_ "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type DataExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DataExportSpec         `json:"spec"`
	Status DataExportImportStatus `json:"status"`
}

// +kubebuilder:object:root=true
type DataExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []DataExport `json:"items"`
}

// +k8s:deepcopy-gen=true
type DataExportSpec struct {
	Ttl     string `json:"ttl"`
	Publish bool   `json:"publish"`
	// PublicIngress selects which cluster ingress to expose public URL through
	// allowed values: KubernetesAPI, ConsoleFrontend
	PublicIngress string                  `json:"publicIngress,omitempty"`
	TargetRef     DataExportTargetRefSpec `json:"targetRef"`
}

// DataExportTargetRefSpec references the export target by GroupKind + name (namespace is implicit = the
// DataExport's own namespace). The version is intentionally NOT pinned: the controller resolves the
// served version and the plural resource via the RESTMapper (RESTMapping on the {group, kind}). Snapshot
// targets (any registered snapshot CR: generic VolumeSnapshot, domain snapshot, ...) are resolved
// generically through the leaf's bound SnapshotContent.dataRef; the live PVC/VirtualDisk direct-export
// targets remain special-cased. A bare cluster-scoped VolumeSnapshotContent is rejected (cross-namespace
// exfiltration).
// +k8s:deepcopy-gen=true
type DataExportTargetRefSpec struct {
	// Group is the API group of the target resource ("" = core group, e.g. for PersistentVolumeClaim).
	Group string `json:"group,omitempty"`
	// Kind is the target object kind (e.g. "PersistentVolumeClaim", "VirtualDisk", "VolumeSnapshot",
	// "VirtualDiskSnapshot").
	Kind string `json:"kind"`
	// Name is the target object name.
	Name string `json:"name"`
}

// func init() {
// 	SchemeBuilder.Register(&DataExport{}, &DataExportList{})
// }

type AuthData struct {
	AuthType   string
	Token      string
	Username   string
	Password   string
	ClientCert x509.Certificate
}

func (de *DataExport) GetStatus() *DataExportImportStatus {
	return &de.Status
}
