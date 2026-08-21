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

const uploadManifests = `[{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data"}}]`

func uploadRequest(group, body string) *http.Request {
	return httptest.NewRequest(http.MethodPost,
		subresourcePath(group, testNamespace, testVSName, SubresourceManifestsUpload),
		strings.NewReader(body))
}

// The facade owns the namespaced half of an import: it decides whether the write is allowed and against
// which content, then forwards ONLY the manifests. Child references never reach the content layer — that
// layer is content-addressed and has no notion of them.
func TestUploadFacade_ForwardsManifestsOnlyAndRelaysTheCoreStatusVerbatim(t *testing.T) {
	coreStatus := []byte(`{"kind":"Status","apiVersion":"v1","status":"Success","manifestCheckpointName":"mcp-reconstructed"}`)
	core := &fakeCore{uploadCode: http.StatusOK, uploadBody: coreStatus}
	reader := newFakeReader().
		withVolumeSnapshot(newVolumeSnapshot(withImportMode())).
		withContent(newSnapshotContent())

	body := fmt.Sprintf(`{"manifests":%s,"childRefs":[]}`, uploadManifests)
	rec := serve(newTestHandler(reader, core), uploadRequest(PrimaryGroup, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(coreStatus) {
		t.Fatalf("core Status was not relayed verbatim:\n got %s\nwant %s", rec.Body.String(), coreStatus)
	}
	if core.uploadedFor != testContent {
		t.Fatalf("forwarded to content %q, want %q", core.uploadedFor, testContent)
	}
	var forwarded interface{}
	if err := json.Unmarshal(core.uploadedManifests, &forwarded); err != nil {
		t.Fatalf("forwarded manifests are not valid JSON: %v", err)
	}
	if _, isArray := forwarded.([]interface{}); !isArray {
		t.Fatalf("forwarded payload is not the manifest array: %s", core.uploadedManifests)
	}
	if strings.Contains(string(core.uploadedManifests), "childRefs") {
		t.Fatalf("childRefs leaked into the content layer payload: %s", core.uploadedManifests)
	}
}

// The core's own error Status is relayed with its code and body untouched: a client keys off the reason
// in it, and re-encoding it here would fork the contract between the two upload layers.
func TestUploadFacade_RelaysACoreErrorStatusUnchanged(t *testing.T) {
	coreStatus := []byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Conflict","code":409,"message":"checkpoint is not ready"}`)
	core := &fakeCore{uploadCode: http.StatusConflict, uploadBody: coreStatus}
	reader := newFakeReader().
		withVolumeSnapshot(newVolumeSnapshot(withImportMode())).
		withContent(newSnapshotContent())

	rec := serve(newTestHandler(reader, core), uploadRequest(PrimaryGroup, fmt.Sprintf(`{"manifests":%s}`, uploadManifests)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409", rec.Code)
	}
	if rec.Body.String() != string(coreStatus) {
		t.Fatalf("core error Status was not relayed verbatim:\n got %s\nwant %s", rec.Body.String(), coreStatus)
	}
}

// Every gate below refuses BEFORE anything is forwarded. The assertion that the content layer was not
// called is the point: a rejection that still wrote would have already changed the import.
func TestUploadFacade_GatesRefuseBeforeForwarding(t *testing.T) {
	foreignRef := &storagev1alpha1.SnapshotSubjectRef{
		APIVersion: sfsnapv1.SchemeGroupVersion.String(),
		Kind:       KindVolumeSnapshot,
		Namespace:  testNamespace,
		Name:       "someone-elses-snapshot",
	}

	for _, tc := range []struct {
		name       string
		reader     *fakeReader
		body       string
		wantStatus int
		wantReason metav1.StatusReason
	}{
		{
			name:       "body is not JSON",
			reader:     newFakeReader().withVolumeSnapshot(newVolumeSnapshot(withImportMode())).withContent(newSnapshotContent()),
			body:       `not json`,
			wantStatus: http.StatusBadRequest,
			wantReason: "BadRequest",
		},
		{
			name:       "manifests missing",
			reader:     newFakeReader().withVolumeSnapshot(newVolumeSnapshot(withImportMode())).withContent(newSnapshotContent()),
			body:       `{"childRefs":[]}`,
			wantStatus: http.StatusBadRequest,
			wantReason: "BadRequest",
		},
		{
			name:       "manifests is not an array",
			reader:     newFakeReader().withVolumeSnapshot(newVolumeSnapshot(withImportMode())).withContent(newSnapshotContent()),
			body:       `{"manifests":{"apiVersion":"v1"}}`,
			wantStatus: http.StatusBadRequest,
			wantReason: "BadRequest",
		},
		{
			name:   "a leaf may not declare children",
			reader: newFakeReader().withVolumeSnapshot(newVolumeSnapshot(withImportMode())).withContent(newSnapshotContent()),
			body: fmt.Sprintf(`{"manifests":%s,"childRefs":[{"apiVersion":"snapshot.storage.k8s.io/v1","kind":"VolumeSnapshot","name":"child"}]}`,
				uploadManifests),
			wantStatus: http.StatusBadRequest,
			wantReason: "BadRequest",
		},
		{
			name:       "addressed VolumeSnapshot does not exist",
			reader:     newFakeReader(),
			body:       fmt.Sprintf(`{"manifests":%s}`, uploadManifests),
			wantStatus: http.StatusNotFound,
			wantReason: "NotFound",
		},
		{
			name:       "snapshot is not in import mode",
			reader:     newFakeReader().withVolumeSnapshot(newVolumeSnapshot()).withContent(newSnapshotContent()),
			body:       fmt.Sprintf(`{"manifests":%s}`, uploadManifests),
			wantStatus: http.StatusConflict,
			wantReason: "Conflict",
		},
		{
			name: "not bound to a SnapshotContent yet",
			reader: newFakeReader().
				withVolumeSnapshot(newVolumeSnapshot(withImportMode(), withoutStatusField("boundSnapshotContentName"))),
			body:       fmt.Sprintf(`{"manifests":%s}`, uploadManifests),
			wantStatus: http.StatusConflict,
			wantReason: ReasonImportContentNotBound,
		},
		{
			name: "bound content does not point back at this snapshot",
			reader: newFakeReader().
				withVolumeSnapshot(newVolumeSnapshot(withImportMode())).
				withContent(newSnapshotContent(withSnapshotRef(foreignRef))),
			body:       fmt.Sprintf(`{"manifests":%s}`, uploadManifests),
			wantStatus: http.StatusForbidden,
			wantReason: "Forbidden",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := &fakeCore{uploadCode: http.StatusOK, uploadBody: []byte(`{"status":"Success"}`)}
			rec := serve(newTestHandler(tc.reader, core), uploadRequest(PrimaryGroup, tc.body))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := decodeStatus(t, rec.Body.Bytes()).Reason; got != tc.wantReason {
				t.Fatalf("reason: got %q want %q", got, tc.wantReason)
			}
			if core.uploadCalls != 0 {
				t.Fatalf("the content layer was called %d times despite a refusal", core.uploadCalls)
			}
		})
	}
}

// The unbound case is the one reason clients wait and retry on, and the string is shared with the
// snapshot core's own upload endpoint. This module cannot import that constant, so the only thing
// keeping the two from drifting is this assertion on the literal value.
func TestUploadFacade_UnboundSnapshotUsesTheSharedNotBoundReason(t *testing.T) {
	if ReasonImportContentNotBound != "ImportContentNotBound" {
		t.Fatalf("the not-bound reason is part of the wire contract: got %q", ReasonImportContentNotBound)
	}

	core := &fakeCore{}
	reader := newFakeReader().
		withVolumeSnapshot(newVolumeSnapshot(withImportMode(), withoutStatusField("boundSnapshotContentName")))

	rec := serve(newTestHandler(reader, core), uploadRequest(PrimaryGroup, fmt.Sprintf(`{"manifests":%s}`, uploadManifests)))

	status := decodeStatus(t, rec.Body.Bytes())
	if rec.Code != http.StatusConflict || status.Reason != "ImportContentNotBound" {
		t.Fatalf("got %d/%q, want 409/ImportContentNotBound (body %s)", rec.Code, status.Reason, rec.Body.String())
	}
}

// A transport failure reaching the snapshot core is an upstream fault. Reporting it as 500 would tell a
// client keying off the status that the fault is on this side and that retrying is pointless.
func TestUploadFacade_UnreachableCoreIsAGatewayError(t *testing.T) {
	core := &fakeCore{uploadErr: fmt.Errorf("connection refused")}
	reader := newFakeReader().
		withVolumeSnapshot(newVolumeSnapshot(withImportMode())).
		withContent(newSnapshotContent())

	rec := serve(newTestHandler(reader, core), uploadRequest(PrimaryGroup, fmt.Sprintf(`{"manifests":%s}`, uploadManifests)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if got := decodeStatus(t, rec.Body.Bytes()).Reason; got != "BadGateway" {
		t.Fatalf("reason: got %q want BadGateway", got)
	}
}
