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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// suppressContentType marks a stub response that must go out with NO Content-Type header. The test server
// adds one by sniffing the body unless the header is explicitly set to nil, and that default is what makes
// an "absent content type" case quietly become a text/plain case.
const suppressContentType = "none\x00"

// contentLayerStub stands in for the snapshot core's aggregated content layer, recording what the client
// actually put on the wire.
type contentLayerStub struct {
	server   *httptest.Server
	requests atomic.Int32
	// lastQuery is the raw query of the last request, which is where a client-go request deadline shows
	// up on the wire.
	lastQuery atomic.Value
}

func newContentLayerStub(t *testing.T, handler http.HandlerFunc) *contentLayerStub {
	t.Helper()
	stub := &contentLayerStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.requests.Add(1)
		stub.lastQuery.Store(r.URL.RawQuery)
		handler(w, r)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *contentLayerStub) client(t *testing.T) *CoreContentClient {
	t.Helper()
	client, err := NewCoreContentClient(&rest.Config{Host: s.server.URL})
	if err != nil {
		t.Fatalf("build the content layer client: %v", err)
	}
	return client
}

// writeStatus answers like the aggregated content layer does: a Kubernetes Status with the code.
func writeStatus(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Reason:   metav1.StatusReason(reason),
		Message:  message,
		Code:     int32(code),
	})
}

// The client is what produces the error the handler then relays, so the fidelity of the whole relay rests
// here: every status the content layer can answer with has to come back out of the client with that same
// status, not flattened into one.
func TestCoreContentClient_CarriesTheContentLayerStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		reason  string
		message string
	}{
		{name: "capture still in flight", code: http.StatusConflict, reason: "Conflict", message: "checkpoint is not ready"},
		{name: "snapshot core restarting", code: http.StatusServiceUnavailable, reason: "ServiceUnavailable", message: "not serving"},
		{name: "malformed address", code: http.StatusBadRequest, reason: "BadRequest", message: "content name is required"},
		{name: "no such content", code: http.StatusNotFound, reason: "NotFound", message: `SnapshotContent "x" not found`},
		{name: "no permission", code: http.StatusForbidden, reason: "Forbidden", message: "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
				writeStatus(w, tc.code, tc.reason, tc.message)
			})

			_, err := stub.client(t).ContentManifestsRaw(context.Background(), testContent)

			var relayed *coreStatusError
			if !errors.As(err, &relayed) {
				t.Fatalf("error does not carry the content layer's status: %#v", err)
			}
			if relayed.code != tc.code {
				t.Fatalf("code: got %d want %d", relayed.code, tc.code)
			}
			if relayed.reason != tc.reason {
				t.Fatalf("reason: got %q want %q", relayed.reason, tc.reason)
			}
			if !strings.Contains(relayed.message, tc.message) {
				t.Fatalf("message lost the content layer's text: %q", relayed.message)
			}
			// The message has to say WHICH content failed, or a multi-node import gives no clue.
			if !strings.Contains(relayed.message, testContent) {
				t.Fatalf("message does not name the content: %q", relayed.message)
			}
		})
	}
}

// A body that is not a Status is the sidecar-proxy case: a proxy in the request path answers text/plain
// or text/html, and the client cannot decode it. It must still relay ITS OWN code.
//
// The codes below are deliberately none of them 502. 502 is what the unreachable-content-layer fallback
// answers with, so a stub returning 502 cannot tell the two apart — the test would pass with the whole
// non-Status branch deleted. Each case here fails unless the branch actually runs.
func TestCoreContentClient_CarriesTheStatusOfANonStatusBody(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		contentType string
		body        string
		wantReason  string
		// wantCodeInMessage pins WHICH half of the branch produced the answer, which the code, the reason
		// and the quoted text alone cannot distinguish. The half that keeps the status code has no Status
		// to quote the code from, so it restates it in the message; the half that recovers the code from
		// the error does not. Without this the two halves are interchangeable to the test, and a case
		// meant for one of them can drift into the other unnoticed.
		wantCodeInMessage bool
	}{
		{
			name: "conflict behind a text/plain proxy", code: http.StatusConflict,
			contentType: "text/plain", body: "upstream conflict", wantReason: "Conflict",
		},
		{
			name: "not found behind a text/plain proxy", code: http.StatusNotFound,
			contentType: "text/plain", body: "no route", wantReason: "NotFound",
		},
		{
			name: "unavailable behind an html proxy", code: http.StatusServiceUnavailable,
			contentType: "text/html", body: "<html>503</html>", wantReason: "ServiceUnavailable",
		},
		{
			// No Content-Type header AT ALL takes the OTHER half of this branch: the client falls back to
			// its configured type, decoding gets far enough to keep the status code, and the body reaches
			// the relay as a non-Status payload with a code. Suppressing the header takes an explicit
			// nil — leaving it unset makes the test server sniff the body and send text/plain, which
			// would quietly turn this case into a copy of the one above and leave that half unrun.
			name: "no content type at all", code: http.StatusBadRequest,
			contentType: suppressContentType, body: "malformed", wantReason: "BadRequest",
			wantCodeInMessage: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.contentType == suppressContentType {
					w.Header()["Content-Type"] = nil
				} else {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			})

			_, err := stub.client(t).ContentManifestsRaw(context.Background(), testContent)

			var relayed *coreStatusError
			if !errors.As(err, &relayed) {
				t.Fatalf("error does not carry a status: %#v", err)
			}
			if relayed.code != tc.code {
				t.Fatalf("code: got %d want %d (message %q)", relayed.code, tc.code, relayed.message)
			}
			if relayed.reason != tc.wantReason {
				t.Fatalf("reason: got %q want %q", relayed.reason, tc.wantReason)
			}
			if !strings.Contains(relayed.message, testContent) {
				t.Fatalf("message does not name the content: %q", relayed.message)
			}
			// The far end's own words are the only explanation of what happened there, and on this path
			// client-go substitutes a phrase of its own for them.
			if !strings.Contains(relayed.message, tc.body) {
				t.Fatalf("message lost the text the far end sent (%q): %q", tc.body, relayed.message)
			}
			if got := strings.Contains(relayed.message, fmt.Sprintf("answered %d", tc.code)); got != tc.wantCodeInMessage {
				t.Fatalf("this case is meant to go through the %s half of the branch, and it did not (message %q)",
					map[bool]string{true: "code-preserved", false: "code-recovered"}[tc.wantCodeInMessage], relayed.message)
			}
		})
	}
}

// The same recovery is owed on the forward, where the status is relayed as a body rather than an error.
func TestCoreContentClient_UploadCarriesTheStatusOfANonStatusBody(t *testing.T) {
	stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("upstream conflict"))
	})

	code, body, err := stub.client(t).UploadContentManifests(context.Background(), testContent, json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("an HTTP status is relayed, not returned as an error: %v", err)
	}
	if code != http.StatusConflict {
		t.Fatalf("relayed code: got %d want 409", code)
	}
	var status metav1.Status
	if uerr := json.Unmarshal(body, &status); uerr != nil {
		t.Fatalf("the relayed body is not a Status: %v (body %s)", uerr, body)
	}
	if status.Reason != "Conflict" || status.Code != int32(http.StatusConflict) {
		t.Fatalf("relayed Status: got %q/%d want Conflict/409", status.Reason, status.Code)
	}
}

// No HTTP response at all is an upstream fault, not ours. Reporting it as 500 would tell a client keying
// off the status that the fault is on this side and that retrying is pointless.
func TestCoreContentClient_UnreachableContentLayerIsAGatewayError(t *testing.T) {
	stub := newContentLayerStub(t, func(http.ResponseWriter, *http.Request) {})
	client := stub.client(t)
	stub.server.Close()

	_, err := client.ContentManifestsRaw(context.Background(), testContent)

	var relayed *coreStatusError
	if !errors.As(err, &relayed) {
		t.Fatalf("error does not carry a status: %#v", err)
	}
	if relayed.code != http.StatusBadGateway || relayed.reason != "BadGateway" {
		t.Fatalf("got %d/%q, want 502/BadGateway", relayed.code, relayed.reason)
	}
}

// Retries are ON by default in client-go — up to ten attempts on 429 and 5xx, honouring Retry-After — so
// "we wrote no retry loop" is not the same as "there are no retries". This facade answers inside a
// caller's deadline, and ten backed-off attempts spend all of it: the caller times out with nothing to
// act on instead of receiving the fast, honest status the first attempt already produced. A stub that
// asks for a retry is the only way to see the difference.
func TestCoreContentClient_DoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{
		{name: "too many requests", code: http.StatusTooManyRequests},
		{name: "service unavailable", code: http.StatusServiceUnavailable},
		{name: "internal error", code: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
				// The invitation to retry that client-go honours on its own.
				w.Header().Set("Retry-After", "1")
				writeStatus(w, tc.code, "", "try again")
			})

			_, err := stub.client(t).ContentManifestsRaw(context.Background(), testContent)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := stub.requests.Load(); got != 1 {
				t.Fatalf("the content layer received %d requests, want exactly 1 (retries are on)", got)
			}
			var relayed *coreStatusError
			if !errors.As(err, &relayed) || relayed.code != tc.code {
				t.Fatalf("the retryable status was not relayed: %#v", err)
			}
		})
	}
}

// A missing reason must not reach a client that switches on it.
func TestCoreContentClient_FillsInAMissingReason(t *testing.T) {
	stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusServiceUnavailable, "", "no backend")
	})

	_, err := stub.client(t).ContentManifestsRaw(context.Background(), testContent)

	var relayed *coreStatusError
	if !errors.As(err, &relayed) {
		t.Fatalf("error does not carry a status: %#v", err)
	}
	if relayed.reason != "ServiceUnavailable" {
		t.Fatalf("reason: got %q want ServiceUnavailable", relayed.reason)
	}
}

// The call carries an explicit deadline. It travels as a query parameter, which is also how the server
// side learns to stop working on an abandoned request — so its presence on the wire is the observable
// form of "this call is bounded".
func TestCoreContentClient_SendsAnExplicitDeadline(t *testing.T) {
	stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})

	if _, err := stub.client(t).ContentManifestsRaw(context.Background(), testContent); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	query, _ := stub.lastQuery.Load().(string)
	want := "timeout=" + coreContentCallTimeout.String()
	if !strings.Contains(query, want) {
		t.Fatalf("the request carries no deadline: query %q, want it to contain %q", query, want)
	}
}

// The upload forward is bounded the same way, and it is the call where an unnoticed retry does the most
// damage: it is the request an import waits on.
func TestCoreContentClient_UploadDoesNotRetryAndCarriesADeadline(t *testing.T) {
	stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		writeStatus(w, http.StatusTooManyRequests, "", "try again")
	})

	code, _, err := stub.client(t).UploadContentManifests(context.Background(), testContent, json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("an HTTP status is relayed, not returned as an error: %v", err)
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("relayed code: got %d want %d", code, http.StatusTooManyRequests)
	}
	if got := stub.requests.Load(); got != 1 {
		t.Fatalf("the content layer received %d requests, want exactly 1 (retries are on)", got)
	}
	query, _ := stub.lastQuery.Load().(string)
	if want := "timeout=" + coreContentCallTimeout.String(); !strings.Contains(query, want) {
		t.Fatalf("the upload carries no deadline: query %q, want it to contain %q", query, want)
	}
}

// The upload has to address the content it resolved. Dropping the name from the path would send every
// node's manifests to the same address.
func TestCoreContentClient_UploadAddressesTheResolvedContent(t *testing.T) {
	var gotPath atomic.Value
	stub := newContentLayerStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"Status","status":"Success"}`))
	})

	if _, _, err := stub.client(t).UploadContentManifests(context.Background(), testContent, json.RawMessage(`[]`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	path, _ := gotPath.Load().(string)
	want := "/apis/" + coreContentGroupVersion.Group + "/" + coreContentGroupVersion.Version +
		"/snapshotcontents/" + testContent + "/manifests-upload"
	if path != want {
		t.Fatalf("upload path: got %q want %q", path, want)
	}
}

// The download addresses the same content, on the read endpoint.
func TestCoreContentClient_DownloadAddressesTheResolvedContent(t *testing.T) {
	var gotPath atomic.Value
	stub := newContentLayerStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})

	if _, err := stub.client(t).ContentManifestsRaw(context.Background(), testContent); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	path, _ := gotPath.Load().(string)
	want := "/apis/" + coreContentGroupVersion.Group + "/" + coreContentGroupVersion.Version +
		"/snapshotcontents/" + testContent + "/" + SubresourceManifestsDownload
	if path != want {
		t.Fatalf("download path: got %q want %q", path, want)
	}
}

// One request spends its budget on up to three sequential calls, and the point of bounding each is that
// the facade gives up before its caller does — a caller that times out first reports only its own
// deadline, with nothing about which dependency stalled. Raising any one deadline until the sum no longer
// fits hands that diagnosis back, silently, so the sum is what is asserted.
func TestFacadeTimeouts_FitInsideACallerBudget(t *testing.T) {
	for name, d := range map[string]time.Duration{
		"kubeObjectReadTimeout":  kubeObjectReadTimeout,
		"coreContentCallTimeout": coreContentCallTimeout,
	} {
		if d <= 0 {
			t.Fatalf("%s is %s: that call is unbounded", name, d)
		}
	}

	// The addressed snapshot, the content it names, then the content layer.
	worst := 2*kubeObjectReadTimeout + coreContentCallTimeout
	if worst >= facadeCallerBudget {
		t.Fatalf("the three calls of one request can take %s, which does not fit inside the %s a caller allows",
			worst, facadeCallerBudget)
	}
}

// The facade's own object reads have to be bounded, and bounded on a COPY: the config they are derived
// from is the manager's, shared with every controller in this process, so a deadline written onto it
// would also cut short their watches.
func TestFacadeReaderConfig_BoundsTheReadsWithoutTouchingTheSharedConfig(t *testing.T) {
	shared := &rest.Config{Host: "https://example.invalid"}

	derived := facadeReaderConfig(shared)

	if derived.Timeout != kubeObjectReadTimeout {
		t.Fatalf("the facade's reads are bounded by %s, want %s", derived.Timeout, kubeObjectReadTimeout)
	}
	if shared.Timeout != 0 {
		t.Fatalf("the shared config was given a %s timeout; every controller's watches in this process would inherit it", shared.Timeout)
	}
	if derived == shared {
		t.Fatal("the derived config is the shared one, so any later change to it leaks across the process")
	}
}

// A failure body that is not a Status must never reach the facade's own client as-is. That client is a
// Kubernetes client too: handed a body it cannot decode under a response that claims JSON, it falls into
// exactly the defect this relay exists to prevent one hop up — with no Status to read, it derives the
// reason from its own request verb, turning a 409 on a POST into an "AlreadyExists" that means nothing
// here.
//
// The two cases are the two halves of the same branch, and they get there differently: a declared
// text/plain makes the client refuse to decode and drop the status code, while an absent Content-Type
// lets it keep the code and hand the body through.
func TestCoreContentClient_UploadNeverRelaysAForeignBody(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "declared text/plain", contentType: "text/plain", body: "proxy said no"},
		{name: "no content type at all", contentType: suppressContentType, body: "proxy said no"},
		{
			// The case the Kind check exists for, and the one the two above cannot see: a gateway answers
			// with WELL-FORMED JSON that simply is not a Status. The code survives, decoding succeeds, and
			// without checking Kind the foreign object goes straight to the facade's client — which finds
			// no reason in it and derives one from its own verb.
			name:        "json that is not a Status",
			contentType: "application/json",
			body:        `{"message":"RBAC: access denied"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.contentType == suppressContentType {
					w.Header()["Content-Type"] = nil
				} else {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(tc.body))
			})

			code, body, err := stub.client(t).UploadContentManifests(context.Background(), testContent, json.RawMessage(`[]`))
			if err != nil {
				t.Fatalf("an HTTP status is relayed, not returned as an error: %v", err)
			}
			if code != http.StatusConflict {
				t.Fatalf("relayed code: got %d want 409", code)
			}

			var status metav1.Status
			if uerr := json.Unmarshal(body, &status); uerr != nil {
				t.Fatalf("the relayed body is not JSON, but the response declares it: %v (body %s)", uerr, body)
			}
			if status.Kind != "Status" {
				t.Fatalf("the relayed body is not a Status, so the facade's client cannot read a reason from it: %s", body)
			}
			if status.Reason != "Conflict" {
				t.Fatalf("reason: got %q want Conflict (a verb-derived reason here would be AlreadyExists)", status.Reason)
			}
			if status.Code != int32(http.StatusConflict) {
				t.Fatalf("Status.code: got %d want 409", status.Code)
			}
			if !strings.Contains(status.Message, tc.body) {
				t.Fatalf("the far end's text was lost: %q", status.Message)
			}
		})
	}
}

// The forward's own unreachable case: no HTTP response at all leaves nothing to relay, so it is the
// caller's error rather than a status. It is the fallback the two recovery paths above lean on, so it is
// exercised rather than assumed.
func TestCoreContentClient_UploadUnreachableContentLayerIsAnError(t *testing.T) {
	stub := newContentLayerStub(t, func(http.ResponseWriter, *http.Request) {})
	client := stub.client(t)
	stub.server.Close()

	code, body, err := client.UploadContentManifests(context.Background(), testContent, json.RawMessage(`[]`))
	if err == nil {
		t.Fatal("expected an error when the content layer cannot be reached")
	}
	if code != 0 || body != nil {
		t.Fatalf("nothing was relayed, so there is no status to report: got code %d, body %s", code, body)
	}
	if !strings.Contains(err.Error(), testContent) {
		t.Fatalf("the error does not name the content: %v", err)
	}
}

// A failure body that is not text either — a binary payload — leaves client-go with nothing to quote, and
// it files the placeholder "unknown" as the cause. The relay must then fall back to describing the code
// rather than passing that placeholder on as if it were the far end's explanation.
//
// The assertion is on the EXPLANATION, not on the whole message: the message always begins with the
// subject ("read the manifests of ..."), so checking that the message is non-empty proves nothing at all
// — it stays true with the entire fallback replaced by "". What has to survive is the part after that
// prefix.
func TestCoreContentClient_FallsBackWhenThereIsNoTextToQuote(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        []byte
		code        int
	}{
		{
			name:        "binary body, code client-go has a phrase for",
			contentType: "application/octet-stream",
			body:        []byte{0x00, 0x01, 0x02},
			code:        http.StatusServiceUnavailable,
		},
		{
			// A code with no dedicated phrase in client-go still gets its generic one, so this stays on
			// the same tier as the case above — it is here because that generic phrase is the only thing
			// left to relay, and dropping it would leave the client with the subject and nothing else.
			name:        "binary body, code client-go has no dedicated phrase for",
			contentType: "application/octet-stream",
			body:        []byte{0xff},
			code:        http.StatusTeapot,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.code)
				_, _ = w.Write(tc.body)
			})

			_, err := stub.client(t).ContentManifestsRaw(context.Background(), testContent)

			var relayed *coreStatusError
			if !errors.As(err, &relayed) {
				t.Fatalf("error does not carry a status: %#v", err)
			}
			if relayed.code != tc.code {
				t.Fatalf("code: got %d want %d", relayed.code, tc.code)
			}
			if strings.Contains(relayed.message, "unknown") {
				t.Fatalf("the placeholder was passed on as an explanation: %q", relayed.message)
			}

			// Everything after "<subject>: " is the explanation the client is left with.
			prefix := readContentSubject(testContent) + ": "
			explanation, ok := strings.CutPrefix(relayed.message, prefix)
			if !ok {
				t.Fatalf("message %q does not start with the subject %q", relayed.message, prefix)
			}
			if strings.TrimSpace(explanation) == "" {
				t.Fatalf("the message explains nothing beyond the subject: %q", relayed.message)
			}
			// It has to say something about what happened, not merely be non-empty: either a phrase or,
			// at the last tier, the status code.
			if len(strings.TrimSpace(explanation)) < len("the server") &&
				!strings.Contains(explanation, strconv.Itoa(tc.code)) {
				t.Fatalf("the explanation is too thin to act on: %q", explanation)
			}
		})
	}
}

// A SUCCESSFUL forward is relayed byte for byte. The content layer's success envelope carries the
// reconstructed checkpoint name, which is a field of that envelope and not of a Kubernetes Status, so
// decoding and re-encoding the body — even into a valid Status — silently drops the one value the client
// made the request for. Asserted against the real client, because the invariant is the client's.
func TestCoreContentClient_UploadRelaysASuccessBodyByteForByte(t *testing.T) {
	// Deliberately not in the shape a Go encoder would produce: a field no Status models, keys out of
	// alphabetical order, and indentation. Any re-encoding on this path changes these bytes.
	success := "{\n  \"status\": \"Success\",\n  \"kind\": \"Status\",\n" +
		"  \"apiVersion\": \"v1\",\n  \"manifestCheckpointName\": \"mcp-reconstructed\"\n}"
	stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(success))
	})

	code, body, err := stub.client(t).UploadContentManifests(context.Background(), testContent, json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("relayed code: got %d want 200", code)
	}
	if string(body) != success {
		t.Fatalf("the success body was not relayed verbatim:\n got %s\nwant %s", body, success)
	}
}

// A SUCCESSFUL read is relayed byte for byte. The manifests travel on to a client that applies them, and
// the download endpoint is defined as returning what was captured — so a re-encode on this path, even one
// that preserves every value, is a change to the objects a restore is built from.
//
// The fixture is deliberately not in the shape a Go encoder produces. Every read fixture used by the
// tests that go through the fake is the canonical "[]", and against that a decode-and-re-encode is
// invisible: the byte-for-byte requirement can only be seen on a payload whose formatting an encoder
// would not reproduce.
func TestCoreContentClient_RelaysASuccessfulReadByteForByte(t *testing.T) {
	manifests := "[\n  {\n    \"kind\": \"PersistentVolumeClaim\",\n    \"apiVersion\": \"v1\",\n" +
		"    \"status\": {\"phase\": \"Bound\"},\n" +
		"    \"metadata\": {\"namespace\": \"app-ns\", \"name\": \"data\", \"uid\": \"pvc-uid-1\"}\n  }\n]"
	stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(manifests))
	})

	raw, err := stub.client(t).ContentManifestsRaw(context.Background(), testContent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(raw) != manifests {
		t.Fatalf("the captured manifests were not relayed verbatim:\n got %s\nwant %s", raw, manifests)
	}
}

// An ERROR body that IS a Status is relayed byte for byte too, and this is the half that is easy to lose:
// the branch that rescues a foreign body must not also rewrite a genuine one. The content layer's own
// Status carries a message and details the client acts on, and rebuilding it from the outside flattens
// both into whatever this facade happens to reconstruct.
func TestCoreContentClient_UploadRelaysAGenuineErrorStatusByteForByte(t *testing.T) {
	// A real Status, formatted as the far end sent it, carrying details a rebuild would drop.
	status := "{\n  \"kind\": \"Status\",\n  \"apiVersion\": \"v1\",\n  \"status\": \"Failure\",\n" +
		"  \"reason\": \"ImportContentNotBound\",\n  \"code\": 409,\n" +
		"  \"message\": \"waiting for the binder\",\n" +
		"  \"details\": {\"name\": \"content-pvc-snap\", \"causes\": [{\"reason\": \"NotBound\", \"message\": \"no content yet\"}]}\n}"
	stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(status))
	})

	code, body, err := stub.client(t).UploadContentManifests(context.Background(), testContent, json.RawMessage(`[]`))
	if err != nil {
		t.Fatalf("an HTTP status is relayed, not returned as an error: %v", err)
	}
	if code != http.StatusConflict {
		t.Fatalf("relayed code: got %d want 409", code)
	}
	if string(body) != status {
		t.Fatalf("a genuine Status was rebuilt instead of relayed:\n got %s\nwant %s", body, status)
	}
}

// The last tier of the message chain: a Status that carries neither a message nor a reason. It is
// reachable only on the path where the body IS a Status — client-go always supplies a phrase of its own on
// the other one — and without this tier the client would be handed the subject followed by nothing, for a
// failure it still has to act on.
func TestCoreContentClient_DescribesTheCodeWhenAStatusSaysNothingElse(t *testing.T) {
	stub := newContentLayerStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		// A Status with a code and nothing to explain it.
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","code":409}`))
	})

	_, err := stub.client(t).ContentManifestsRaw(context.Background(), testContent)

	var relayed *coreStatusError
	if !errors.As(err, &relayed) {
		t.Fatalf("error does not carry a status: %#v", err)
	}
	if relayed.code != http.StatusConflict {
		t.Fatalf("code: got %d want 409", relayed.code)
	}
	// The reason is still filled in from the code, so a client switching on it is not handed "".
	if relayed.reason != "Conflict" {
		t.Fatalf("reason: got %q want Conflict", relayed.reason)
	}
	explanation, ok := strings.CutPrefix(relayed.message, readContentSubject(testContent)+": ")
	if !ok {
		t.Fatalf("message %q does not start with the subject", relayed.message)
	}
	if !strings.Contains(explanation, strconv.Itoa(http.StatusConflict)) {
		t.Fatalf("with nothing else to say, the message must name the code: %q", explanation)
	}
}
