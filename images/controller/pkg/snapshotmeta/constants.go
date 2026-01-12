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

// Package snapshotmeta provides constants for Deckhouse snapshot annotations and labels
// used for integration between storage-foundation and patched snapshot-controller.
// These constants MUST be identical in both storage-foundation and snapshot-controller
// to ensure proper integration.
package snapshotmeta

// Deckhouse annotation constants for snapshot-controller integration
// These annotations tell snapshot-controller that VSC/VS are managed by Deckhouse
// and should not trigger CSI CreateSnapshot/DeleteSnapshot calls
const (
	// AnnDeckhouseManaged marks VolumeSnapshotContent or VolumeSnapshot as managed by Deckhouse
	// When present, snapshot-controller skips CSI CreateSnapshot/DeleteSnapshot calls
	// and sets ReadyToUse=true without actual snapshot creation
	AnnDeckhouseManaged = "storage.deckhouse.io/managed"

	// AnnDeckhouseSourceSnapshotContent contains the name of the source SnapshotContent (VCR name)
	// Used by snapshot-controller to find VSC by SFC name
	// Value: only name (not namespace/name), as snapshot-controller compares only name
	AnnDeckhouseSourceSnapshotContent = "storage.deckhouse.io/source-snapshot-content"

	// AnnDeckhouseSourcePVC contains the source PVC reference
	// Format: "namespace/name" (e.g., "default/my-pvc")
	// Used by snapshot-controller to get PVC size for RestoreSize
	AnnDeckhouseSourcePVC = "storage.deckhouse.io/source-pvc"

	// AnnDeckhouseVCRUID is set on PVC to signal snapshot-controller to create VSC
	// Format: VCR UID (string)
	// When snapshot-controller sees this annotation on PVC, it creates VSC with proper ownerRefs
	AnnDeckhouseVCRUID = "volumesnapshot.deckhouse.io/vcr"
)

// Deckhouse label constants for proxy objects
const (
	// LabelDeckhouseProxy marks VolumeSnapshot as a proxy object
	// When set to "true", indicates that VS is a proxy for VSC and should not trigger VSC creation
	LabelDeckhouseProxy = "storage.deckhouse.io/proxy"
)
