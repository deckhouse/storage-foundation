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

// +k8s:deepcopy-gen=true
// ObjectReference references a Kubernetes object
type ObjectReference struct {
	// Name is the name of the referenced object
	Name string `json:"name"`
	// Namespace is the namespace of the referenced object (optional)
	Namespace string `json:"namespace,omitempty"`
	// Kind is the kind of the referenced object (e.g., "VolumeSnapshotContent", "PersistentVolume")
	Kind string `json:"kind,omitempty"`
}

