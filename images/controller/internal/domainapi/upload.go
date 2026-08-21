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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReasonImportContentNotBound is the status.reason of the 409 an upload gets while the addressed import
// snapshot has no bound SnapshotContent yet. It is a transport-level "wait for the binder", not a
// Ready-condition reason, and clients retry on it — so the string is part of the wire contract and MUST
// stay identical to the one the snapshot core's own upload endpoint returns. This module cannot import
// that constant (it is internal to the other module), which is exactly why the value is repeated here
// rather than derived, and why TestUploadFacade_UnboundSnapshotUsesTheSharedNotBoundReason pins it.
const ReasonImportContentNotBound = "ImportContentNotBound"

// manifestsAndChildrenUpload is the per-node import payload: one node's own manifests plus the references
// to its direct children. The envelope is the shared one, so a client writes the same body whichever
// server owns the node.
type manifestsAndChildrenUpload struct {
	// Manifests is a JSON array of raw objects — the same shape manifests-download returns. Forwarded
	// verbatim to the content layer.
	Manifests json.RawMessage `json:"manifests"`

	// ChildRefs lists the direct children of this node. A VolumeSnapshot is a data leaf and has none, so
	// the only accepted value here is empty (see Upload).
	ChildRefs []uploadChildRef `json:"childRefs"`
}

// uploadChildRef is one direct-child reference in an import payload (namespace is implicit — the
// addressed object's).
type uploadChildRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// facadeStatusError is a fail-closed rejection raised BEFORE the content layer is reached (bad payload,
// wrong mode, not bound, back-reference mismatch). It carries the exact HTTP status and reason the
// handler must emit. Errors that originate in the content layer are never wrapped in it: those are
// relayed verbatim.
type facadeStatusError struct {
	code    int
	reason  string
	message string
}

func (e *facadeStatusError) Error() string { return e.message }

func newFacadeStatusError(code int, reason, message string) *facadeStatusError {
	return &facadeStatusError{code: code, reason: reason, message: message}
}

// UploadFacade serves manifests-and-children-refs-upload for a VolumeSnapshot. It is the write
// counterpart of the download proxy and shares its resolve contract, so an upload can no more be aimed at
// a foreign content than a read can.
type UploadFacade struct {
	// reader is uncached: mode and bind state decide whether the write is allowed at all, and a stale read
	// of either would let it through.
	reader client.Reader
	core   CoreContentManifests
	log    logr.Logger
}

// NewUploadFacade builds the write half of the facade. reader must be uncached.
func NewUploadFacade(reader client.Reader, core CoreContentManifests, log logr.Logger) *UploadFacade {
	return &UploadFacade{reader: reader, core: core, log: log}
}

// Upload persists one VolumeSnapshot's import payload by forwarding its manifests to the content layer.
// It returns the content layer's HTTP status code and raw body to relay VERBATIM, or a
// *facadeStatusError for a rejection made before the forward.
//
// The order of the gates is the shared one and each step depends on the previous:
//
//  1. the payload is well formed, and its child list is empty — a VolumeSnapshot is a data leaf, so a
//     non-empty list means the client mistook it for an inner node and the rest of its import would be
//     built on that mistake;
//  2. the addressed snapshot exists and is in import mode — an upload must never overwrite the manifests
//     of a live capture;
//  3. it is bound to a SnapshotContent — the binder works independently of upload, so until it has caught
//     up the answer is the shared retryable 409 rather than a write to nowhere;
//  4. that content points back at this very snapshot — status.boundSnapshotContentName is writable by
//     whoever writes the snapshot's status, so without the reverse check an import could be steered into
//     someone else's content.
//
// Nothing is written to the addressed object itself. The one thing this layer would own on it is the
// child list, and for a leaf that list is empty by definition: writing it would mean contending for the
// object's status with its own controller in order to store nothing.
func (f *UploadFacade) Upload(ctx context.Context, namespace, name string, body []byte) (int, []byte, error) {
	var payload manifestsAndChildrenUpload
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, nil, newFacadeStatusError(http.StatusBadRequest, "BadRequest", fmt.Sprintf("invalid upload payload: %v", err))
	}
	manifests, ferr := validateUploadManifests(payload.Manifests)
	if ferr != nil {
		return 0, nil, ferr
	}
	if ferr := validateLeafChildRefs(payload.ChildRefs, namespace, name); ferr != nil {
		return 0, nil, ferr
	}

	// Classified by the SAME table the reads use, so a snapshot that cannot be read answers identically
	// whichever subresource asked.
	id, err := loadAddressedVolumeSnapshot(ctx, f.reader, namespace, name)
	if err != nil {
		return 0, nil, resolveStatusFor(SubresourceManifestsUpload, err)
	}
	if !id.importMode {
		return 0, nil, newFacadeStatusError(http.StatusConflict, "Conflict",
			fmt.Sprintf("VolumeSnapshot %s/%s is not in import mode (spec.mode is not Import): refusing manifests upload", namespace, name))
	}

	// Resolve the bound content with the same handshake the read path uses, from the observation of the
	// snapshot already made above. Its "not bound yet" is re-stated with the shared retryable reason,
	// which is what a client waits on.
	content, err := resolveBoundContentFor(ctx, f.reader, id)
	if err != nil {
		return 0, nil, resolveStatusFor(SubresourceManifestsUpload, err)
	}

	code, respBody, uerr := f.core.UploadContentManifests(ctx, content.Name, manifests)
	if uerr != nil {
		// The snapshot core was unreachable — an upstream failure, not ours. 502 with a matching reason:
		// "InternalError" canonically maps to 500 and would tell a client keying off the reason that the
		// fault is on this side. 502 is more precise than the 503 the published status table lists for a
		// non-serving aggregated API, and it is documented separately from that table for that reason.
		return 0, nil, newFacadeStatusError(http.StatusBadGateway, "BadGateway",
			fmt.Sprintf("forward manifests to SnapshotContent %q: %v", content.Name, uerr))
	}
	f.log.V(1).Info("forwarded manifests-and-children-refs-upload",
		"namespace", namespace, "name", name, "content", content.Name, "coreStatus", code)
	return code, respBody, nil
}

// validateUploadManifests requires manifests to be present and a JSON array, returning the original bytes
// for forwarding. Absent, null and non-array values are all rejected here so a malformed body never
// reaches the content layer.
func validateUploadManifests(manifests json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(manifests)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, newFacadeStatusError(http.StatusBadRequest, "BadRequest", "manifests is required")
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, newFacadeStatusError(http.StatusBadRequest, "BadRequest",
			fmt.Sprintf("manifests must be a JSON array: %v", err))
	}
	return manifests, nil
}

// validateLeafChildRefs rejects any declared child. A VolumeSnapshot is a data leaf: it has no snapshot
// children, so a child list is not "extra information to ignore" but evidence that the client believes
// this node has a subtree — and silently accepting it would leave that subtree unimported with nothing to
// show for it.
func validateLeafChildRefs(refs []uploadChildRef, namespace, name string) error {
	if len(refs) == 0 {
		return nil
	}
	return newFacadeStatusError(http.StatusBadRequest, "BadRequest",
		fmt.Sprintf("VolumeSnapshot %s/%s is a leaf: childRefs must be empty", namespace, name))
}
