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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	sfsnapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
)

// manifests-download is defined as a byte proxy. Anything the facade did to the payload — reformatting,
// re-encoding, dropping status — would break the import path, which reads the ORIGINAL PVC manifest back
// out of this response.
func TestManifestsDownload_ProxiesContentLayerBytesVerbatim(t *testing.T) {
	// Deliberately NOT in the shape a Go encoder would produce: keys out of alphabetical order and
	// indentation in the payload. Any decode-and-re-encode on this path — even one that preserves every
	// value — changes these bytes, which is what makes the assertion below able to see it at all.
	raw := []byte("[\n  {\n    \"kind\": \"PersistentVolumeClaim\",\n    \"apiVersion\": \"v1\",\n" +
		"    \"status\": {\"phase\": \"Bound\"},\n" +
		"    \"metadata\": {\"namespace\": \"app-ns\", \"name\": \"data\"}\n  }\n]")
	core := &fakeCore{manifests: raw}
	reader := newFakeReader().withVolumeSnapshot(newVolumeSnapshot()).withContent(newSnapshotContent())

	rec := serve(newTestHandler(reader, core),
		httptest.NewRequest(http.MethodGet, subresourcePath(PrimaryGroup, testNamespace, testVSName, SubresourceManifestsDownload), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatalf("body was not proxied verbatim:\n got %s\nwant %s", rec.Body.Bytes(), raw)
	}
	if core.uploadedFor != testContent {
		t.Fatalf("addressed content: got %q want %q", core.uploadedFor, testContent)
	}
}

// The download deliberately has NO readiness gate: it returns what was captured, exactly like the
// per-CR download of the snapshot core it proxies. Adding one here would break export of a snapshot
// that is mid-recapture, which is a legitimate thing to export.
func TestManifestsDownload_DoesNotGateOnReadiness(t *testing.T) {
	core := &fakeCore{manifests: capturedPVCJSON()}
	reader := newFakeReader().
		withVolumeSnapshot(newVolumeSnapshot(withStatusField(false, "readyToUse"))).
		withContent(newSnapshotContent(withContentNotReady()))

	rec := serve(newTestHandler(reader, core),
		httptest.NewRequest(http.MethodGet, subresourcePath(PrimaryGroup, testNamespace, testVSName, SubresourceManifestsDownload), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// The compiled output must carry the PVC bound to the snapshot that holds its data. A PVC emitted
// without that binding is accepted by the API and provisioned as an EMPTY volume, so this is the single
// assertion the whole restore path exists for.
func TestManifestsWithDataRestoration_BindsThePVCToItsVolumeSnapshot(t *testing.T) {
	core := &fakeCore{manifests: capturedPVCJSON()}
	reader := newFakeReader().withVolumeSnapshot(newVolumeSnapshot()).withContent(newSnapshotContent())

	rec := serve(newTestHandler(reader, core),
		httptest.NewRequest(http.MethodGet, subresourcePath(PrimaryGroup, testNamespace, testVSName, SubresourceManifestsRestore), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	objects := decodeObjects(t, rec.Body.Bytes())
	if len(objects) != 1 {
		t.Fatalf("compiled objects: got %d want 1 (%v)", len(objects), objects)
	}
	pvc := objects[0]
	ref, ok := nestedMap(pvc, "spec", "dataSourceRef")
	if !ok {
		t.Fatalf("PVC has no spec.dataSourceRef; it would restore as an empty volume: %v", pvc)
	}
	if got := ref["kind"]; got != KindVolumeSnapshot {
		t.Fatalf("dataSourceRef.kind: got %v want %q", got, KindVolumeSnapshot)
	}
	if got := ref["name"]; got != testVSName {
		t.Fatalf("dataSourceRef.name: got %v want %q", got, testVSName)
	}
	if got := ref["apiGroup"]; got != sfsnapv1.GroupName {
		t.Fatalf("dataSourceRef.apiGroup: got %v want %q", got, sfsnapv1.GroupName)
	}
	// The output is apply-ready: server-managed metadata and status must be gone.
	if _, present := pvc["status"]; present {
		t.Fatalf("compiled PVC still carries status: %v", pvc)
	}
	meta, _ := nestedMap(pvc, "metadata")
	if _, present := meta["resourceVersion"]; present {
		t.Fatalf("compiled PVC still carries metadata.resourceVersion: %v", meta)
	}
}

// The output is addressed into the namespace of the node addressed, never into the one the manifests were
// captured in: applying them as captured would recreate the source namespace's objects, and for a tree
// loaded back into a cluster that namespace may not even exist. The caller does not get to name a third
// one — a restored claim reaches its data through a dataSourceRef that carries no namespace, so the claim
// and the VolumeSnapshot it names have to end up in the same place.
func TestManifestsWithDataRestoration_AddressesTheOutputIntoTheAddressedNamespace(t *testing.T) {
	core := &fakeCore{manifests: capturedPVCJSON()}
	reader := newFakeReader().withVolumeSnapshot(newVolumeSnapshot()).withContent(newSnapshotContent())

	rec := serve(newTestHandler(reader, core), httptest.NewRequest(http.MethodGet,
		subresourcePath(PrimaryGroup, testNamespace, testVSName, SubresourceManifestsRestore), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	objects := decodeObjects(t, rec.Body.Bytes())
	if len(objects) != 1 {
		t.Fatalf("compiled objects: got %d want 1 — with nothing emitted the namespace assertion below checks nothing (%v)", len(objects), objects)
	}
	meta, _ := nestedMap(objects[0], "metadata")
	if got := meta["namespace"]; got != testNamespace {
		t.Fatalf("compiled PVC namespace: got %v, want the addressed %q; it was captured in %q, so a value other than the addressed one means the restore rewrite is gone",
			got, testNamespace, capturedNamespace)
	}
}

// A caller can still put a target namespace on the wire, and this facade ANSWERS such a request: 200, with
// the output addressed into the snapshot's own namespace regardless of what was asked for. The snapshot
// core refuses the same request with 400, so one request means two things depending on the group.
//
// That disagreement does NOT end by itself. The refusal is a separate function of the shared restore
// package, which the query parser does not call; it begins to act when the handler calls it, and not when
// a dependency is raised. So this guard has two jobs. It reddens on the day someone wires the refusal up
// — the intended end state, and the moment to rewrite it to assert the refusal instead — and until then it
// is the only thing stopping the value from quietly being obeyed again, which takes two edits here, in the
// handler and in the compilation, and which no other test would see: none of them sends the value.
func TestManifestsWithDataRestoration_AnswersACallerSuppliedTargetNamespaceInTheAddressedOne(t *testing.T) {
	core := &fakeCore{manifests: capturedPVCJSON()}
	reader := newFakeReader().withVolumeSnapshot(newVolumeSnapshot()).withContent(newSnapshotContent())

	path := subresourcePath(PrimaryGroup, testNamespace, testVSName, SubresourceManifestsRestore) + "?targetNamespace=other-ns"
	rec := serve(newTestHandler(reader, core), httptest.NewRequest(http.MethodGet, path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body %s); a 400 means the shared refusal was wired into the handler — that is the intended end state, so replace this guard with one asserting the refusal. Nothing else produces it: raising the dependency without adding that call leaves the answer at 200",
			rec.Code, rec.Body.String())
	}
	objects := decodeObjects(t, rec.Body.Bytes())
	if len(objects) != 1 {
		t.Fatalf("compiled objects: got %d want 1 — with nothing emitted the namespace assertion below checks nothing (%v)", len(objects), objects)
	}
	meta, _ := nestedMap(objects[0], "metadata")
	if got := meta["namespace"]; got != testNamespace {
		t.Fatalf("compiled PVC namespace: got %v, want the addressed %q; a value on the wire must not move where the answer is addressed (it was captured in %q)",
			got, testNamespace, capturedNamespace)
	}
}

// The compilation is fail-closed on a PVC with no data binding: without one there is nothing to point
// spec.dataSourceRef at, and emitting the PVC anyway would restore an empty volume silently.
func TestManifestsWithDataRestoration_RefusesAPVCWithNoDataBinding(t *testing.T) {
	core := &fakeCore{manifests: capturedPVCJSON()}
	reader := newFakeReader().
		withVolumeSnapshot(newVolumeSnapshot()).
		withContent(newSnapshotContent(withoutDataBinding()))

	rec := serve(newTestHandler(reader, core),
		httptest.NewRequest(http.MethodGet, subresourcePath(PrimaryGroup, testNamespace, testVSName, SubresourceManifestsRestore), nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

// Failures that answer the same way on every subresource. A single catch-all would make "not yours"
// indistinguishable from "not there".
func TestReadSubresources_MapFailuresToStatuses(t *testing.T) {
	foreignRef := &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: sfsnapv1.SchemeGroupVersion.String(),
		Kind:       KindVolumeSnapshot,
		Namespace:  testNamespace,
		Name:       "someone-elses-snapshot",
	}

	for _, tc := range []struct {
		name       string
		reader     *fakeReader
		wantStatus int
		wantReason metav1.StatusReason
	}{
		{
			name:       "addressed VolumeSnapshot does not exist",
			reader:     newFakeReader(),
			wantStatus: http.StatusNotFound,
			wantReason: "NotFound",
		},
		{
			name: "bound content does not point back at this snapshot",
			reader: newFakeReader().
				withVolumeSnapshot(newVolumeSnapshot()).
				withContent(newSnapshotContent(withSnapshotRef(foreignRef))),
			wantStatus: http.StatusForbidden,
			wantReason: "Forbidden",
		},
		{
			name: "bound content carries no back-reference at all",
			reader: newFakeReader().
				withVolumeSnapshot(newVolumeSnapshot()).
				withContent(newSnapshotContent(withSnapshotRef(nil))),
			wantStatus: http.StatusForbidden,
			wantReason: "Forbidden",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, subresource := range []string{SubresourceManifestsDownload, SubresourceManifestsRestore} {
				core := &fakeCore{manifests: capturedPVCJSON()}
				rec := serve(newTestHandler(tc.reader, core),
					httptest.NewRequest(http.MethodGet, subresourcePath(PrimaryGroup, testNamespace, testVSName, subresource), nil))

				if rec.Code != tc.wantStatus {
					t.Fatalf("%s status: got %d want %d (body %s)", subresource, rec.Code, tc.wantStatus, rec.Body.String())
				}
				if got := decodeStatus(t, rec.Body.Bytes()).Reason; got != tc.wantReason {
					t.Fatalf("%s reason: got %q want %q", subresource, got, tc.wantReason)
				}
			}
		})
	}
}

// Two resolve outcomes answer DIFFERENTLY per subresource, and the differences are the contract clients
// already act on — a read of a volume node that is not bound yet is a 400 they must not treat as fatal,
// while the same condition on an import is the 409 they wait on. Every cell is asserted, because getting
// one wrong is invisible until a client in the field gives up on a request that would have succeeded.
func TestResolveStatuses_MatchThePerSubresourceContract(t *testing.T) {
	unbound := func() *fakeReader {
		return newFakeReader().withVolumeSnapshot(
			newVolumeSnapshot(withImportMode(), withoutStatusField("boundSnapshotContentName")))
	}
	// A bound name pointing at a content that is not there.
	dangling := func() *fakeReader {
		return newFakeReader().withVolumeSnapshot(newVolumeSnapshot(withImportMode()))
	}

	for _, tc := range []struct {
		condition   string
		reader      func() *fakeReader
		subresource string
		wantStatus  int
		wantReason  metav1.StatusReason
	}{
		{"not bound yet", unbound, SubresourceManifestsDownload, http.StatusBadRequest, "BadRequest"},
		{"not bound yet", unbound, SubresourceManifestsRestore, http.StatusConflict, "Conflict"},
		{"not bound yet", unbound, SubresourceManifestsUpload, http.StatusConflict, ReasonImportContentNotBound},
		{"bound content absent", dangling, SubresourceManifestsDownload, http.StatusNotFound, "NotFound"},
		{"bound content absent", dangling, SubresourceManifestsRestore, http.StatusConflict, "Conflict"},
		{"bound content absent", dangling, SubresourceManifestsUpload, http.StatusNotFound, "NotFound"},
	} {
		t.Run(tc.condition+" on "+tc.subresource, func(t *testing.T) {
			core := &fakeCore{manifests: capturedPVCJSON()}
			handler := newTestHandler(tc.reader(), core)

			var req *http.Request
			if tc.subresource == SubresourceManifestsUpload {
				req = uploadRequest(PrimaryGroup, fmt.Sprintf(`{"manifests":%s}`, uploadManifests))
			} else {
				req = httptest.NewRequest(http.MethodGet,
					subresourcePath(PrimaryGroup, testNamespace, testVSName, tc.subresource), nil)
			}

			rec := serve(handler, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := decodeStatus(t, rec.Body.Bytes()).Reason; got != tc.wantReason {
				t.Fatalf("reason: got %q want %q", got, tc.wantReason)
			}
		})
	}
}

// The status the snapshot core's content layer answered with is what the client is documented to act on,
// and this facade is the only thing between the two. Collapsing it — reporting a capture still in flight
// or a restarting core as an internal error — tells the client to give up on a request that would have
// succeeded seconds later.
func TestReadSubresources_RelayTheContentLayerStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fetchErr   error
		wantStatus int
		wantReason metav1.StatusReason
	}{
		{
			name:       "capture still in flight",
			fetchErr:   &coreStatusError{code: http.StatusConflict, reason: "Conflict", message: "checkpoint is not ready"},
			wantStatus: http.StatusConflict,
			wantReason: "Conflict",
		},
		{
			name:       "snapshot core is restarting",
			fetchErr:   &coreStatusError{code: http.StatusServiceUnavailable, reason: "ServiceUnavailable", message: "not serving"},
			wantStatus: http.StatusServiceUnavailable,
			wantReason: "ServiceUnavailable",
		},
		{
			name:       "malformed address",
			fetchErr:   &coreStatusError{code: http.StatusBadRequest, reason: "BadRequest", message: "content name is required"},
			wantStatus: http.StatusBadRequest,
			wantReason: "BadRequest",
		},
		{
			name:       "content layer unreachable",
			fetchErr:   &coreStatusError{code: http.StatusBadGateway, reason: "BadGateway", message: "connection refused"},
			wantStatus: http.StatusBadGateway,
			wantReason: "BadGateway",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := newFakeReader().withVolumeSnapshot(newVolumeSnapshot()).withContent(newSnapshotContent())
			for _, subresource := range []string{SubresourceManifestsDownload, SubresourceManifestsRestore} {
				core := &fakeCore{fetchErr: tc.fetchErr}
				rec := serve(newTestHandler(reader, core),
					httptest.NewRequest(http.MethodGet, subresourcePath(PrimaryGroup, testNamespace, testVSName, subresource), nil))

				if rec.Code != tc.wantStatus {
					t.Fatalf("%s status: got %d want %d (body %s)", subresource, rec.Code, tc.wantStatus, rec.Body.String())
				}
				status := decodeStatus(t, rec.Body.Bytes())
				if status.Reason != tc.wantReason {
					t.Fatalf("%s reason: got %q want %q", subresource, status.Reason, tc.wantReason)
				}
				// The relayed message is the only diagnostic the client gets about what actually failed.
				if !strings.Contains(status.Message, tc.fetchErr.Error()) {
					t.Fatalf("%s message lost the content layer's own text: %q", subresource, status.Message)
				}
			}
		})
	}
}

// Restore adds gates the download does not have, and every one of them prevents handing out a manifest
// that would restore wrong or absent data.
func TestManifestsWithDataRestoration_GatesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader *fakeReader
	}{
		{
			name: "snapshot is being deleted",
			reader: newFakeReader().
				withVolumeSnapshot(newVolumeSnapshot(withDeletionTimestamp())).
				withContent(newSnapshotContent()),
		},
		{
			name: "snapshot is not readyToUse",
			reader: newFakeReader().
				withVolumeSnapshot(newVolumeSnapshot(withStatusField(false, "readyToUse"))).
				withContent(newSnapshotContent()),
		},
		{
			name: "no durable volume content bound",
			reader: newFakeReader().
				withVolumeSnapshot(newVolumeSnapshot(withoutStatusField("boundVolumeSnapshotContentName"))).
				withContent(newSnapshotContent()),
		},
		{
			name: "bound SnapshotContent is not Ready",
			reader: newFakeReader().
				withVolumeSnapshot(newVolumeSnapshot()).
				withContent(newSnapshotContent(withContentNotReady())),
		},
		{
			name: "bound SnapshotContent has no manifest checkpoint",
			reader: newFakeReader().
				withVolumeSnapshot(newVolumeSnapshot()).
				withContent(newSnapshotContent(withoutCheckpoint())),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := &fakeCore{manifests: capturedPVCJSON()}
			rec := serve(newTestHandler(tc.reader, core),
				httptest.NewRequest(http.MethodGet, subresourcePath(PrimaryGroup, testNamespace, testVSName, SubresourceManifestsRestore), nil))

			if rec.Code != http.StatusConflict {
				t.Fatalf("status: got %d want 409 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// The object filter is how a client restores ONE object out of a node, and scope=node is the only scope
// it is valid with. These cases assert that the facade CALLS the shared query parser rather than reading
// the parameters itself: an implementation that ignored them would answer 200 with the whole node here,
// and 200 instead of 400 on a scope it never heard of.
func TestManifestsWithDataRestoration_UsesTheSharedQueryContract(t *testing.T) {
	newHandler := func() *Handler {
		return newTestHandler(
			newFakeReader().withVolumeSnapshot(newVolumeSnapshot()).withContent(newSnapshotContent()),
			&fakeCore{manifests: capturedPVCJSON()},
		)
	}
	base := subresourcePath(PrimaryGroup, testNamespace, testVSName, SubresourceManifestsRestore)

	t.Run("scope=node with an object filter narrows to that object", func(t *testing.T) {
		rec := serve(newHandler(), httptest.NewRequest(http.MethodGet,
			base+"?scope=node&kind=PersistentVolumeClaim&name="+testPVCName, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d want 200 (body %s)", rec.Code, rec.Body.String())
		}
		objects := decodeObjects(t, rec.Body.Bytes())
		if len(objects) != 1 || objectKey(objects[0]) != "v1/PersistentVolumeClaim/"+testNamespace+"/"+testPVCName {
			t.Fatalf("filtered output: got %v", objects)
		}
	})

	t.Run("a filter that matches nothing is a 404, not an empty list", func(t *testing.T) {
		rec := serve(newHandler(), httptest.NewRequest(http.MethodGet,
			base+"?scope=node&kind=ConfigMap&name=absent", nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status: got %d want 404 (body %s)", rec.Code, rec.Body.String())
		}
	})

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "unknown scope", query: "?scope=everything"},
		{name: "kind without name", query: "?scope=node&kind=PersistentVolumeClaim"},
		{name: "name without kind", query: "?scope=node&name=" + testPVCName},
		{name: "apiVersion without kind", query: "?scope=node&apiVersion=v1"},
		{name: "object filter with scope=subtree", query: "?scope=subtree&kind=PersistentVolumeClaim&name=" + testPVCName},
	} {
		t.Run("rejected: "+tc.name, func(t *testing.T) {
			rec := serve(newHandler(), httptest.NewRequest(http.MethodGet, base+tc.query, nil))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if got := decodeStatus(t, rec.Body.Bytes()).Reason; got != "BadRequest" {
				t.Fatalf("reason: got %q want BadRequest", got)
			}
		})
	}
}

// An empty compilation must serialize as `[]`. A client applies the concatenation of several servers'
// answers, and a `null` in the middle makes the whole document unparseable as a list.
func TestManifestsWithDataRestoration_EmptyOutputIsAnEmptyArray(t *testing.T) {
	// A cluster-scoped object is dropped by the restore sanitizer, leaving nothing to emit.
	core := &fakeCore{manifests: []byte(`[{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"app-ns"}}]`)}
	reader := newFakeReader().withVolumeSnapshot(newVolumeSnapshot()).withContent(newSnapshotContent())

	rec := serve(newTestHandler(reader, core),
		httptest.NewRequest(http.MethodGet, subresourcePath(PrimaryGroup, testNamespace, testVSName, SubresourceManifestsRestore), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]" {
		t.Fatalf("empty output: got %q want %q", got, "[]")
	}
}

func decodeObjects(t *testing.T, body []byte) []map[string]interface{} {
	t.Helper()
	var objects []map[string]interface{}
	if err := json.Unmarshal(body, &objects); err != nil {
		t.Fatalf("response is not a JSON array of objects: %v (body %s)", err, body)
	}
	return objects
}

func nestedMap(obj map[string]interface{}, fields ...string) (map[string]interface{}, bool) {
	current := obj
	for _, field := range fields {
		next, ok := current[field].(map[string]interface{})
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}
