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
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/restoreapi"
)

// maxManifestsUploadBytes bounds an import upload body. It is a single node's own manifests, never a
// whole subtree, and it mirrors the per-CR limit of the layer this facade forwards to so a body that
// would be rejected there is rejected here first.
const maxManifestsUploadBytes = 64 << 20 // 64 MiB

// Handler serves both aggregated groups: discovery, and the three namespaced VolumeSnapshot connector
// subresources. Every route is registered once per group and lands in the same method, so the two groups
// cannot drift apart in behaviour; the group only decides what discovery reports and whether the response
// carries the deprecation Warning.
type Handler struct {
	restore *RestoreService
	upload  *UploadFacade
	log     logr.Logger
}

// NewHandler builds the aggregated API handler. upload may be nil in tests that exercise only the
// read-only subresources; the upload subresource then answers "not configured" (500) rather than
// panicking.
func NewHandler(restore *RestoreService, upload *UploadFacade, log logr.Logger) *Handler {
	return &Handler{restore: restore, upload: upload, log: log}
}

// SetupRoutes registers the routes of every served group on mux.
//
// /healthz, /readyz, /livez and /version are deliberately absent: genericapiserver owns them and never
// forwards them here (it also exempts them from authorization), so registering them would create dead
// routes that only look like the live ones.
func (h *Handler) SetupRoutes(mux *http.ServeMux) {
	for _, group := range servedGroups() {
		g := group
		// Group discovery, exact path, no trailing slash.
		mux.HandleFunc(g.rootPath(), func(w http.ResponseWriter, r *http.Request) {
			h.handleGroupDiscovery(w, r, g)
		})
		// Everything below: version discovery and the namespaced subresources.
		mux.HandleFunc(g.subtreePath(), func(w http.ResponseWriter, r *http.Request) {
			h.handleSubtree(w, r, g)
		})
	}
}

// handleGroupDiscovery serves GET /apis/<group> (APIGroup). It reports the group that was ADDRESSED: a
// client reading discovery of the deprecated alias must see the alias, or its own cached discovery and the
// paths it then builds would disagree.
func (h *Handler) handleGroupDiscovery(w http.ResponseWriter, r *http.Request, g servedGroup) {
	h.markDeprecated(w, g)
	if !h.requireMethod(w, r, http.MethodGet) {
		return
	}
	if strings.TrimSuffix(r.URL.Path, "/") != g.rootPath() {
		h.writeError(w, http.StatusNotFound, "NotFound", "resource not found")
		return
	}
	h.writeJSON(w, metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{Kind: "APIGroup", APIVersion: "v1"},
		Name:     g.name,
		Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: g.groupVersion(), Version: GroupVersion},
		},
		PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: g.groupVersion(), Version: GroupVersion},
	})
}

// handleSubtree dispatches everything under /apis/<group>/: version discovery and the namespaced
// subresources.
func (h *Handler) handleSubtree(w http.ResponseWriter, r *http.Request, g servedGroup) {
	h.markDeprecated(w, g)

	rel := strings.TrimPrefix(r.URL.Path, g.subtreePath())
	rel = strings.TrimSuffix(rel, "/")

	if rel == GroupVersion {
		h.handleResourceListDiscovery(w, r, g)
		return
	}
	if !strings.HasPrefix(rel, namespacedPrefix()) {
		h.writeError(w, http.StatusNotFound, "NotFound", "resource not found")
		return
	}

	parts := strings.Split(strings.TrimPrefix(rel, namespacedPrefix()), "/")
	if len(parts) != 4 {
		// The resource is virtual: it has no storage, so there is nothing to read at
		// namespaces/<ns>/volumesnapshots/<name> and nothing to list one level up. Only a
		// <name>/<subresource> address exists.
		h.writeError(w, http.StatusNotFound, "NotFound",
			"expected namespaces/<namespace>/"+ResourceVolumeSnapshots+"/<name>/<subresource>")
		return
	}
	namespace, resource, name, subresource := parts[0], parts[1], parts[2], parts[3]
	if resource != ResourceVolumeSnapshots {
		h.writeError(w, http.StatusNotFound, "NotFound", "unsupported resource")
		return
	}
	// Reject anything that is not a valid namespace/object name before it is spliced into a request path
	// built for the snapshot core (defence in depth against traversal and injection).
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		h.writeError(w, http.StatusBadRequest, "BadRequest", "invalid namespace")
		return
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		h.writeError(w, http.StatusBadRequest, "BadRequest", "invalid name")
		return
	}

	switch subresource {
	case SubresourceManifestsDownload:
		if !h.requireMethod(w, r, http.MethodGet) {
			return
		}
		h.handleManifestsDownload(w, r, namespace, name)
	case SubresourceManifestsRestore:
		if !h.requireMethod(w, r, http.MethodGet) {
			return
		}
		h.handleManifestsWithDataRestoration(w, r, namespace, name)
	case SubresourceManifestsUpload:
		if !h.requireMethod(w, r, http.MethodPost) {
			return
		}
		h.handleManifestsAndChildrenUpload(w, r, namespace, name)
	default:
		h.writeError(w, http.StatusNotFound, "NotFound", "unknown subresource")
	}
}

// handleResourceListDiscovery serves GET /apis/<group>/<version> (APIResourceList). Only the three
// subresources are advertised: there is no listable or gettable bare volumesnapshots resource here, its
// storage is the CSI CRD.
func (h *Handler) handleResourceListDiscovery(w http.ResponseWriter, r *http.Request, g servedGroup) {
	if !h.requireMethod(w, r, http.MethodGet) {
		return
	}
	subresource := func(name string, verbs ...string) metav1.APIResource {
		return metav1.APIResource{
			Name:       ResourceVolumeSnapshots + "/" + name,
			Namespaced: true,
			Kind:       KindVolumeSnapshot,
			Verbs:      verbs,
		}
	}
	h.writeJSON(w, metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
		GroupVersion: g.groupVersion(),
		APIResources: []metav1.APIResource{
			subresource(SubresourceManifestsDownload, "get"),
			subresource(SubresourceManifestsRestore, "get"),
			subresource(SubresourceManifestsUpload, "create"),
		},
	})
}

// handleManifestsDownload proxies a node's own captured manifests through verbatim (raw, with status, no
// sanitization and no readiness gate) — the export/import read of a single VolumeSnapshot.
func (h *Handler) handleManifestsDownload(w http.ResponseWriter, r *http.Request, namespace, name string) {
	data, err := h.restore.NodeManifestsDownload(r.Context(), namespace, name)
	if err != nil {
		h.writeReadError(w, err)
		return
	}
	h.writeManifests(w, r, data)
}

// handleManifestsWithDataRestoration compiles the apply-ready restore output of one VolumeSnapshot.
func (h *Handler) handleManifestsWithDataRestoration(w http.ResponseWriter, r *http.Request, namespace, name string) {
	// The query contract (scope/kind/name/apiVersion and the four ways they can contradict each other) is
	// the shared one, parsed by the same code the snapshot core's own handler uses, so one query means one
	// thing on every group that serves this subresource. A VolumeSnapshot is a leaf, which makes
	// scope=node and scope=subtree equivalent here — but an object filter still applies, and that is the
	// path a client takes to restore a single object.
	opts, err := restoreapi.ParseQueryOptions(r)
	if err != nil {
		restoreapi.WriteError(w, err)
		return
	}
	opts.SnapshotNamespace = namespace
	opts.SnapshotName = name
	opts.TargetNamespace = r.URL.Query().Get("targetNamespace")

	data, err := h.restore.ManifestsWithDataRestoration(r.Context(), opts)
	if err != nil {
		h.writeReadError(w, err)
		return
	}
	h.writeManifests(w, r, data)
}

// handleManifestsAndChildrenUpload serves the import upload facade: it size-bounds the body, delegates to
// the facade, and relays the snapshot core's own Status to the client unchanged.
func (h *Handler) handleManifestsAndChildrenUpload(w http.ResponseWriter, r *http.Request, namespace, name string) {
	if h.upload == nil {
		h.writeError(w, http.StatusInternalServerError, "InternalError", "upload facade not configured")
		return
	}
	body, ok := h.readUploadBody(w, r)
	if !ok {
		return
	}
	code, respBody, err := h.upload.Upload(r.Context(), namespace, name, body)
	if err != nil {
		h.writeUploadError(w, err)
		return
	}
	// Relay verbatim: the core's Success carries the reconstructed checkpoint name a client keys off, and
	// its errors carry reasons a client keys off as well. Re-encoding either would fork the contract.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(respBody); err != nil {
		h.log.V(1).Info("write upload response failed", "namespace", namespace, "name", name, "error", err)
	}
}

// readUploadBody reads and size-bounds an upload body, writing the response and returning ok=false on
// failure.
func (h *Handler) readUploadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxManifestsUploadBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.writeError(w, http.StatusRequestEntityTooLarge, "RequestEntityTooLarge",
				fmt.Sprintf("upload body exceeds %d bytes", maxManifestsUploadBytes))
			return nil, false
		}
		h.writeError(w, http.StatusBadRequest, "BadRequest", fmt.Sprintf("read upload body: %v", err))
		return nil, false
	}
	return body, true
}

// writeReadError answers a failed read with the status that failure carries.
//
// The order is the priority of who decided the status. A status RELAYED from the snapshot core's content
// layer wins: it is the answer the client is documented to act on, and this facade is the only thing
// between the two, so collapsing it loses it for good. Next comes a status this facade decided itself
// (the per-subresource resolve table, the payload gates). Only an error that carries no status of its own
// falls through to the shared restore mapping, which classifies the compilation sentinels and defaults
// anything unclassified to an internal fault — never to a client mistake.
func (h *Handler) writeReadError(w http.ResponseWriter, err error) {
	var relayed *coreStatusError
	if errors.As(err, &relayed) {
		h.writeError(w, relayed.code, relayed.reason, relayed.message)
		return
	}
	var decided *facadeStatusError
	if errors.As(err, &decided) {
		h.writeError(w, decided.code, decided.reason, decided.message)
		return
	}
	restoreapi.WriteError(w, err)
}

// writeUploadError answers a failed upload. It shares writeReadError's ordering; it differs only in the
// fallback, because an upload rejection that reached here without a status is a fault of this facade
// (every gate it enforces attaches one), not something the restore vocabulary can classify.
func (h *Handler) writeUploadError(w http.ResponseWriter, err error) {
	var relayed *coreStatusError
	if errors.As(err, &relayed) {
		h.writeError(w, relayed.code, relayed.reason, relayed.message)
		return
	}
	var decided *facadeStatusError
	if errors.As(err, &decided) {
		h.writeError(w, decided.code, decided.reason, decided.message)
		return
	}
	h.writeError(w, http.StatusInternalServerError, "InternalError", err.Error())
}

// markDeprecated adds the Warning header to a response of the deprecated alias group. It runs before
// anything is written, because a header set after the first write never reaches the client.
func (h *Handler) markDeprecated(w http.ResponseWriter, g servedGroup) {
	if g.deprecated {
		w.Header().Add("Warning", deprecationWarning)
	}
}

// requireMethod enforces the HTTP method, writing 405 and returning false on mismatch.
func (h *Handler) requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		h.writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only "+method+" is supported")
		return false
	}
	return true
}

// writeManifests answers with a manifest array, compressing when the client asked for it. The bytes are
// identical whichever group was addressed.
func (h *Handler) writeManifests(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		_, _ = gz.Write(data)
		return
	}
	_, _ = w.Write(data)
}

func (h *Handler) writeJSON(w http.ResponseWriter, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (h *Handler) writeError(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Reason:   metav1.StatusReason(reason),
		Code:     int32(code),
	})
}
