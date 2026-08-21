/*
Copyright 2026 Flant JSC

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

package domainapi

const (
	// PrimaryGroup is this module's aggregated subresources group. It is named after the MODULE, not after
	// the kind it currently serves: the group belongs to the module and will host the subresources of this
	// module's own CRDs as well, under the version of their own main group. It is deliberately distinct
	// from the CSI CRD group snapshot.storage.k8s.io, where the real VolumeSnapshot and
	// VolumeSnapshotContent objects live, so serving it cannot intercept those CRDs.
	PrimaryGroup = "subresources.storage-foundation.deckhouse.io"

	// DeprecatedAliasGroup is a second group served by the very same routes and handlers, kept so clients
	// released on a different schedule keep working across the rename to PrimaryGroup. A request to it is
	// answered byte-for-byte like the same request to PrimaryGroup; the only differences are the group name
	// in discovery (a client must never be told it is talking to a group it did not address) and a Warning
	// header pointing at PrimaryGroup.
	//
	// There is no client-side fallback anywhere: compatibility is held by this server, whose release we
	// control, and not by clients, whose release we do not. The alias goes away once every released client
	// addresses PrimaryGroup — see the APIService template for what removal consists of.
	DeprecatedAliasGroup = "subresources.snapshot.storage.k8s.io"

	// GroupVersion is the version BOTH groups are served under. It follows the version of the served
	// resource's main group — the CSI VolumeSnapshot is v1 — and not the version of this module's own
	// CRDs.
	GroupVersion = "v1"

	// ResourceVolumeSnapshots is the virtual resource the subresources hang off. It has NO storage of its
	// own: the objects live in the CSI CRD group, and addressing this resource without a subresource is a
	// NotFound rather than a read of anything.
	ResourceVolumeSnapshots = "volumesnapshots"

	// KindVolumeSnapshot is the kind discovery reports for the virtual resource — the real CSI kind the
	// subresources are keyed by.
	KindVolumeSnapshot = "VolumeSnapshot"

	// The three connector subresources. Their names are part of the wire contract shared with the snapshot
	// core's own aggregated group, so a client that learned them there addresses them identically here.
	SubresourceManifestsDownload = "manifests-download"
	SubresourceManifestsRestore  = "manifests-with-data-restoration"
	SubresourceManifestsUpload   = "manifests-and-children-refs-upload"
)

// deprecationWarning is the RFC 7234 warn-text served on every response of DeprecatedAliasGroup, in the
// shape Kubernetes clients already surface to users (`299 - "…"`, the code for a persistent miscellaneous
// warning). It names the group to move to, because a warning that only says "deprecated" leaves the
// reader with nothing to act on.
const deprecationWarning = `299 - "` + DeprecatedAliasGroup + ` is deprecated; use ` + PrimaryGroup + `/` + GroupVersion + ` instead"`

// servedGroup is one aggregated group this server answers for. Both entries below share every handler:
// the group name reaching the handlers is what discovery reports, and it is the ONLY value that may
// influence a response body.
type servedGroup struct {
	// name is the API group as it appears in the request path and in discovery.
	name string
	// deprecated marks the alias, whose responses additionally carry the Warning header. It is a separate
	// field rather than a comparison against DeprecatedAliasGroup so that the handlers never have to know
	// which of the groups is the alias.
	deprecated bool
}

// servedGroups is the full set of groups registered by SetupRoutes. Adding a group here is all it takes
// to serve it; removing the alias entry (with its APIService template) is all it takes to retire it.
func servedGroups() []servedGroup {
	return []servedGroup{
		{name: PrimaryGroup},
		{name: DeprecatedAliasGroup, deprecated: true},
	}
}

// groupVersionPath renders the "<group>/<version>" identity used in discovery documents.
func (g servedGroup) groupVersion() string {
	return g.name + "/" + GroupVersion
}

// rootPath is the group discovery path: /apis/<group>.
func (g servedGroup) rootPath() string {
	return "/apis/" + g.name
}

// subtreePath is the prefix everything below group discovery is dispatched from: /apis/<group>/.
func (g servedGroup) subtreePath() string {
	return g.rootPath() + "/"
}

// namespacedPrefix is the relative prefix of a namespaced request inside subtreePath:
// "<version>/namespaces/".
func namespacedPrefix() string {
	return GroupVersion + "/namespaces/"
}
