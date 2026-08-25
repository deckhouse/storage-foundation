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

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
)

// RestoreService serves the two read subresources of a VolumeSnapshot: the raw manifests-download proxy
// and the compiled manifests-with-data-restoration.
//
// A VolumeSnapshot is a data LEAF: it has no snapshot children, so there is no recursion here and
// scope=node and scope=subtree describe the same output. Everything the compilation needs comes from two
// places — the captured objects, fetched from the snapshot core's content layer, and the bound
// SnapshotContent object, read directly for its data binding.
type RestoreService struct {
	// reader is uncached: bind state and readiness are read fresh on every request, because a stale
	// "not bound yet" would answer a request that has since become serviceable, and a stale "ready" would
	// hand out manifests of a node that is mid-recapture.
	reader client.Reader
	core   CoreContentManifests
	log    logr.Logger
}

// NewRestoreService builds the read half of the facade. reader must be uncached.
func NewRestoreService(reader client.Reader, core CoreContentManifests, log logr.Logger) *RestoreService {
	return &RestoreService{reader: reader, core: core, log: log}
}

// NodeManifestsDownload serves manifests-download as a pure PROXY: it resolves the addressed
// VolumeSnapshot's bound SnapshotContent (with the anti-spoofing handshake) and returns the bytes of the
// content layer's own download verbatim — raw, with status, no sanitization, no rewriting, and no
// readiness gate. The export and import paths depend on getting back exactly what was captured.
func (s *RestoreService) NodeManifestsDownload(ctx context.Context, namespace, name string) ([]byte, error) {
	_, content, err := resolveBoundContent(ctx, s.reader, namespace, name)
	if err != nil {
		return nil, resolveStatusFor(SubresourceManifestsDownload, err)
	}
	raw, err := s.core.ContentManifestsRaw(ctx, content.Name)
	if err != nil {
		return nil, err
	}
	s.log.V(1).Info("proxied manifests-download",
		"namespace", namespace, "name", name, "content", content.Name, "bytes", len(raw))
	return raw, nil
}

// ManifestsWithDataRestoration compiles the apply-ready restore output of one VolumeSnapshot: the PVC it
// captured, pointed at this very snapshot as its data source.
//
// The gates below are fail-closed and each of them prevents handing out a manifest that would restore
// WRONG or EMPTY data, which is why none of them degrades to a partial answer:
//   - being deleted: the data is on its way out;
//   - not readyToUse, or no durable content bound yet: there is no volume to point the PVC at;
//   - bound SnapshotContent not Ready: its manifests may still be mid-capture;
//   - back-reference mismatch: the content is not this snapshot's (handled by the shared resolve).
//
// The compilation itself is the shared one, identical to what the snapshot core runs for the nodes it
// compiles in-process. That is deliberate and load-bearing: a client applies the concatenation of this
// output and the core's, and a second implementation of the PVC-to-snapshot binding that dropped one rule
// would emit a PVC with no data source — accepted by the API and provisioned as an empty volume, with the
// loss visible only when someone reads the data back.
func (s *RestoreService) ManifestsWithDataRestoration(ctx context.Context, opts transform.Options) ([]byte, error) {
	namespace, name := opts.SnapshotNamespace, opts.SnapshotName

	id, content, err := resolveBoundContent(ctx, s.reader, namespace, name)
	if err != nil {
		return nil, resolveStatusFor(SubresourceManifestsRestore, err)
	}
	if id.deleting {
		return nil, fmt.Errorf("%w: VolumeSnapshot %s/%s is being deleted", transform.ErrContractViolation, namespace, name)
	}
	if !id.readyToUse {
		return nil, fmt.Errorf("%w: VolumeSnapshot %s/%s is not readyToUse", transform.ErrNotReady, namespace, name)
	}
	if id.boundVSCName == "" {
		return nil, fmt.Errorf("%w: VolumeSnapshot %s/%s has no status.boundVolumeSnapshotContentName",
			transform.ErrNotReady, namespace, name)
	}
	if !meta.IsStatusConditionTrue(content.Status.Conditions, storagev1alpha1.ConditionReady) {
		return nil, fmt.Errorf("%w: SnapshotContent %s bound by VolumeSnapshot %s/%s is not Ready",
			transform.ErrNotReady, content.Name, namespace, name)
	}
	if content.Status.ManifestCheckpointName == "" {
		return nil, fmt.Errorf("%w: SnapshotContent %s bound by VolumeSnapshot %s/%s has no manifest checkpoint",
			transform.ErrContractViolation, content.Name, namespace, name)
	}

	raw, err := s.core.ContentManifestsRaw(ctx, content.Name)
	if err != nil {
		return nil, err
	}
	captured, err := decodeManifestArray(raw)
	if err != nil {
		return nil, err
	}

	objects, err := transform.CompileNodeManifests(transform.NodeRestoreInput{
		CapturedManifests: captured,
		// The output is addressed into the namespace of the node addressed, and there is no other
		// choice: a restored claim reaches its data through a dataSourceRef that carries no namespace,
		// while the VolumeSnapshot it names stays with the snapshot. The field is named for a target a
		// caller once picked; nothing picks it any more, so this node's own namespace goes in directly
		// rather than through a variable that would read like a decision.
		TargetNamespace: namespace,
		PVCDataBindings: pvcDataBindings(content.Status.Data),
		// This snapshot IS the visibility leaf of the volume its captured PVC used: the durable
		// VolumeSnapshotContent it is bound to is the artifact the data binding names, and this
		// namespaced handle is what a restored PVC can actually reference.
		VolumeSnapshotByContentName: map[string]string{id.boundVSCName: name},
		// No covered PVCs: this facade recreates nothing itself, so every PVC in the node must be backed
		// by a data binding or the compilation fails closed.
		DomainCoveredPVCNames: nil,
	})
	if err != nil {
		return nil, err
	}

	if opts.FilterKind != "" {
		objects, err = transform.FilterSingleObject(objects, opts, fmt.Sprintf("%s/%s", namespace, name))
		if err != nil {
			return nil, err
		}
	}

	s.log.V(1).Info("compiled manifests-with-data-restoration",
		"namespace", namespace, "name", name, "content", content.Name, "objects", len(objects))
	return marshalObjects(objects)
}

// pvcDataBindings projects the bound content's data binding onto the compilation's view of it. The
// cardinality is at most one by design — a node carries at most one data artifact — so this is a 0- or
// 1-element slice. The volume metadata carried alongside (mode, filesystem, storage class, size) is not
// passed on: restore compilation only needs to know WHICH artifact backs WHICH PVC, and everything else
// is read back from the captured manifest.
func pvcDataBindings(data *storagev1alpha1.SnapshotDataBinding) []transform.PVCDataBinding {
	if data == nil {
		return nil
	}
	return []transform.PVCDataBinding{{
		TargetUID: string(data.SourceRef.UID),
		Target: storagev1alpha1.ObjectRef{
			APIVersion: data.SourceRef.APIVersion,
			Kind:       data.SourceRef.Kind,
			Name:       data.SourceRef.Name,
			Namespace:  data.SourceRef.Namespace,
			UID:        string(data.SourceRef.UID),
		},
		Artifact: storagev1alpha1.ObjectRef{
			APIVersion: data.ArtifactRef.APIVersion,
			Kind:       data.ArtifactRef.Kind,
			Name:       data.ArtifactRef.Name,
			UID:        string(data.ArtifactRef.UID),
		},
	}}
}

// decodeManifestArray decodes the content layer's JSON array of captured objects.
func decodeManifestArray(raw []byte) ([]unstructured.Unstructured, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var list []unstructured.Unstructured
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("%w: captured manifests of the addressed node are not a JSON array: %v",
			transform.ErrContractViolation, err)
	}
	return list, nil
}

// marshalObjects serializes compiled objects as a JSON array, refusing a duplicate identity.
//
// The empty result is `[]` and never `null`: a client applies the concatenation of several servers'
// answers, and one of them answering null makes the whole document unparseable as a list. The duplicate
// check is fail-closed for the same reason as everywhere else on this path — two objects of one identity
// mean one of them silently wins on apply.
func marshalObjects(objects []unstructured.Unstructured) ([]byte, error) {
	seen := make(map[string]struct{}, len(objects))
	raw := make([]map[string]interface{}, 0, len(objects))
	for i := range objects {
		obj := objects[i]
		namespace := obj.GetNamespace()
		if namespace == "" {
			namespace = "_cluster"
		}
		key := fmt.Sprintf("%s|%s|%s|%s", obj.GetAPIVersion(), obj.GetKind(), namespace, obj.GetName())
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("%w: duplicate object %s", transform.ErrContractViolation, key)
		}
		seen[key] = struct{}{}
		raw = append(raw, obj.Object)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("serialize compiled manifests: %w", err)
	}
	return data, nil
}
