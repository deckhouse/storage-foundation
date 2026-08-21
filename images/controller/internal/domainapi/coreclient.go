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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// The facade answers inside a budget its caller allows it, and one request spends that budget on up to
// three sequential calls: reading the addressed snapshot, reading the content it names, and the call into
// the snapshot core's content layer. Bounding each of them separately is what keeps a single unresponsive
// dependency from consuming the whole budget: a caller that times out reports nothing but its own
// deadline, while a facade that gives up first reports WHICH dependency stalled and on which object.
//
// The sum of the three is deliberately below facadeCallerBudget; TestFacadeTimeouts_FitInsideACallerBudget
// keeps it that way, because raising any single one until the sum no longer fits silently hands the
// diagnosis back to the caller.
const (
	// facadeCallerBudget is the time a delegating caller allows one call into this facade.
	facadeCallerBudget = 30 * time.Second
	// kubeObjectReadTimeout bounds ONE object read this facade makes (the addressed snapshot, the content
	// it names). Both go through the aggregation layer to the kube-apiserver.
	kubeObjectReadTimeout = 5 * time.Second
	// coreContentCallTimeout bounds the ONE call into the snapshot core's content layer.
	coreContentCallTimeout = 15 * time.Second
)

// coreContentGroupVersion is the snapshot core's aggregated CONTENT layer: cluster-scoped subresources
// addressed by SnapshotContent name. This facade never reads a ManifestCheckpoint itself — it asks the
// owner of that layer for the bytes, over the kube-apiserver aggregation layer under this pod's own
// ServiceAccount, which is also where the request is authorized.
var coreContentGroupVersion = schema.GroupVersion{
	Group:   "subresources.state-snapshotter.deckhouse.io",
	Version: "v1alpha1",
}

// CoreContentManifests is the facade's whole dependency on the snapshot core's content layer: read a
// node's own manifests, and write a node's own manifests.
type CoreContentManifests interface {
	// ContentManifestsRaw returns the raw JSON-array body of
	// snapshotcontents/<contentName>/manifests-download verbatim — the node's captured objects, with
	// status and namespace as stored.
	ContentManifestsRaw(ctx context.Context, contentName string) ([]byte, error)

	// UploadContentManifests POSTs a manifests-only body to
	// snapshotcontents/<contentName>/manifests-upload and returns the core's HTTP status code and raw
	// body VERBATIM, so the caller can relay them unchanged. err is set ONLY when no HTTP response was
	// obtained at all (transport failure); an HTTP error status is returned as (code, body, nil).
	UploadContentManifests(ctx context.Context, contentName string, manifests json.RawMessage) (int, []byte, error)
}

// contentManifestsUploadBody is the wire body of the cluster-scoped upload: manifests only. Child
// references are an attribute of the namespaced layer and are deliberately absent here — the facade
// settles them against its own object and forwards nothing but manifests.
type contentManifestsUploadBody struct {
	Manifests json.RawMessage `json:"manifests"`
}

// CoreContentClient implements CoreContentManifests over the in-cluster REST client.
type CoreContentClient struct {
	rc rest.Interface
}

// NewCoreContentClient builds the content-layer client from an in-cluster rest.Config.
func NewCoreContentClient(cfg *rest.Config) (*CoreContentClient, error) {
	cfgCopy := rest.CopyConfig(cfg)
	// APIPath/GroupVersion/NegotiatedSerializer only satisfy rest.RESTClientFor; every request below
	// builds its path with AbsPath, so these do not shape the URL.
	cfgCopy.APIPath = "/apis"
	gv := coreContentGroupVersion
	cfgCopy.GroupVersion = &gv
	cfgCopy.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	rc, err := rest.RESTClientFor(cfgCopy)
	if err != nil {
		return nil, fmt.Errorf("build snapshot core content client: %w", err)
	}
	return &CoreContentClient{rc: rc}, nil
}

// ContentManifestsRaw fetches GET
// /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/snapshotcontents/<contentName>/manifests-download.
//
// A failure is returned as a *coreStatusError so the caller can pass the content layer's own status
// through to the client instead of collapsing it (see that type).
func (c *CoreContentClient) ContentManifestsRaw(ctx context.Context, contentName string) ([]byte, error) {
	if c == nil || c.rc == nil {
		return nil, fmt.Errorf("snapshot core content client is not configured")
	}
	result := c.rc.Get().
		AbsPath(
			"/apis", coreContentGroupVersion.Group, coreContentGroupVersion.Version,
			"snapshotcontents", contentName, SubresourceManifestsDownload,
		).
		// No automatic retries and an explicit deadline: see coreContentCallTimeout and the
		// MaxRetries note on UploadContentManifests.
		MaxRetries(0).
		Timeout(coreContentCallTimeout).
		Do(ctx)

	// The status code and the raw body are taken separately, exactly as on the upload path, and for the
	// same reason: the error client-go builds for a raw request describes the code in its own generic
	// words ("the server reported a conflict") and does not carry the body. The body is the Status the
	// content layer actually sent, and its message is the only description of what went wrong there.
	var code int
	result.StatusCode(&code)
	raw, rawErr := result.Raw()
	return relayReadResult(readContentSubject(contentName), code, raw, rawErr)
}

// relayReadResult turns one content-layer read into either its bytes or the status to answer with.
//
// The zero code is not only "nothing answered". When a response carries an error status AND a content
// type the client cannot decode — text/plain or text/html, which is exactly what a sidecar proxy in the
// request path returns — client-go hands back a result with NO status code and NO body, keeping the code
// only inside the error it built. Treating that as unreachable is how every failure of the content layer
// turns into one indistinguishable gateway error the moment a proxy is in the path, which is the case
// this relay exists for. So the error is asked for its status before that conclusion is drawn.
func relayReadResult(what string, code int, body []byte, err error) ([]byte, error) {
	if code == 0 {
		if relayed, ok := statusFromError(what, err); ok {
			return nil, relayed
		}
		return nil, unreachableCoreError(what, err)
	}
	if code >= http.StatusBadRequest {
		return nil, relayedCoreError(what, code, body)
	}
	return body, nil
}

// statusFromError recovers a relayed status from the error client-go built when it decoded the response
// itself instead of handing it over. ok is false when the error describes no HTTP response at all.
//
// Neither the reason nor the message on that error can be used as they are, and both substitutions are
// deliberate:
//
//   - the reason is derived from the CODE. This path is reached only when the response carried no
//     Kubernetes Status, so no reason was ever sent; the one on the error is client-go's guess from the
//     request VERB, and that guess is wrong here — it renders a 409 on the upload forward as
//     "AlreadyExists", which for this API means nothing and reads as a claim about the object.
//   - the message is taken from the error's Details.Causes, not from its Message. client-go replaces the
//     response text with a phrase of its own ("the server reported a conflict") and files the actual text
//     under a cause of type UnexpectedServerResponse. That text is the only sentence explaining what
//     happened at the far end, so reporting the generic phrase instead would leave a client with a code
//     and nothing else.
func statusFromError(what string, err error) (*coreStatusError, bool) {
	var status apierrors.APIStatus
	if err == nil || !errors.As(err, &status) {
		return nil, false
	}
	s := status.Status()
	code := int(s.Code)
	if code < http.StatusBadRequest {
		return nil, false
	}
	return &coreStatusError{
		code:    code,
		reason:  relayedReason("", code),
		message: fmt.Sprintf("%s: %s", what, upstreamText(s, code)),
	}, true
}

// upstreamText renders what the far end actually said, preferring the response text client-go filed under
// an UnexpectedServerResponse cause over the phrase it substituted for it. It falls back to that phrase,
// and finally to the code, so the message is never empty.
func upstreamText(s metav1.Status, code int) string {
	if s.Details != nil {
		for _, cause := range s.Details.Causes {
			if cause.Type != metav1.CauseTypeUnexpectedServerResponse {
				continue
			}
			// "unknown" is the placeholder client-go uses when the response was not text it could quote.
			if text := strings.TrimSpace(cause.Message); text != "" && text != "unknown" {
				return text
			}
		}
	}
	return statusMessage(s, code)
}

// readContentSubject names what a failed read was reading. A multi-node import fails one node at a time,
// so the answer has to say which.
func readContentSubject(contentName string) string {
	return fmt.Sprintf("read the manifests of SnapshotContent %q", contentName)
}

// coreStatusError carries a failure of a call into the snapshot core's content layer TOGETHER WITH the
// status that layer answered with.
//
// It exists because those statuses are the ones the client is documented to act on, and they are not
// interchangeable: a capture still in flight answers 409 and clears on its own, a restarting snapshot
// core answers 503 and clears on its own, a malformed address answers 400 and never clears. Reporting
// any of them as a plain internal error tells a client to give up on a request that would have
// succeeded seconds later — and this facade is the only thing standing between the two, so a status it
// drops is a status nobody can recover.
type coreStatusError struct {
	code    int
	reason  string
	message string
}

func (e *coreStatusError) Error() string { return e.message }

// relayedCoreError builds the answer for an HTTP failure of the content layer: its own code, and its own
// reason and message when the body is the Status it normally sends. A body that is not a Status (a proxy
// in front of the aggregation layer, say) still contributes its code, which is the part a client acts on.
func relayedCoreError(what string, code int, body []byte) *coreStatusError {
	var status metav1.Status
	if err := json.Unmarshal(body, &status); err == nil && status.Kind == "Status" {
		return &coreStatusError{
			code:    code,
			reason:  relayedReason(string(status.Reason), code),
			message: fmt.Sprintf("%s: %s", what, statusMessage(status, code)),
		}
	}
	return &coreStatusError{
		code:    code,
		reason:  relayedReason("", code),
		message: fmt.Sprintf("%s: the snapshot core answered %d: %s", what, code, strings.TrimSpace(string(body))),
	}
}

// unreachableCoreError is the answer when no HTTP response was obtained at all: an upstream fault (502),
// matching how the upload path reports the same condition. 500 would tell a client keying off the status
// that the fault is on this side and that retrying is pointless.
func unreachableCoreError(what string, err error) *coreStatusError {
	return &coreStatusError{
		code:    http.StatusBadGateway,
		reason:  "BadGateway",
		message: fmt.Sprintf("%s: %v", what, err),
	}
}

// relayedReason keeps the content layer's own reason, falling back to the canonical reason for the code
// when it sent none. A client that switches on the reason must not be handed an empty string, and the
// codes below are the ones the subresource contract names.
func relayedReason(reason string, code int) string {
	if reason != "" {
		return reason
	}
	switch code {
	case http.StatusBadRequest:
		return "BadRequest"
	case http.StatusForbidden:
		return "Forbidden"
	case http.StatusNotFound:
		return "NotFound"
	case http.StatusConflict:
		return "Conflict"
	case http.StatusRequestEntityTooLarge:
		return "RequestEntityTooLarge"
	case http.StatusTooManyRequests:
		return "TooManyRequests"
	case http.StatusServiceUnavailable:
		return "ServiceUnavailable"
	default:
		return "InternalError"
	}
}

// statusMessage renders a relayed Status for the message, falling back to the reason and then to the code
// when the status carried no message of its own.
func statusMessage(s metav1.Status, code int) string {
	if s.Message != "" {
		return s.Message
	}
	if s.Reason != "" {
		return string(s.Reason)
	}
	return fmt.Sprintf("the snapshot core answered %d", code)
}

// UploadContentManifests forwards manifests-only to POST
// /apis/subresources.state-snapshotter.deckhouse.io/v1alpha1/snapshotcontents/<contentName>/manifests-upload.
// That layer is content-addressed and has no bind gate of its own, so a missing content comes back as a
// plain 404 to relay as-is.
func (c *CoreContentClient) UploadContentManifests(ctx context.Context, contentName string, manifests json.RawMessage) (int, []byte, error) {
	if c == nil || c.rc == nil {
		return 0, nil, fmt.Errorf("snapshot core content client is not configured")
	}
	reqBody, err := json.Marshal(contentManifestsUploadBody{Manifests: manifests})
	if err != nil {
		return 0, nil, fmt.Errorf("marshal manifests-upload body: %w", err)
	}
	result := c.rc.Post().
		AbsPath(
			"/apis", coreContentGroupVersion.Group, coreContentGroupVersion.Version,
			"snapshotcontents", contentName, "manifests-upload",
		).
		SetHeader("Content-Type", "application/json").
		Body(reqBody).
		// MaxRetries(0) is load-bearing, not a default being restated: client-go retries a request up to
		// ten times on 429 and 5xx while honouring Retry-After, so simply not writing a retry loop
		// leaves retries ON. This facade is called inside a caller's deadline, and ten backed-off
		// attempts spend all of it — turning a fast, honest failure into the caller timing out with no
		// status to act on. The deadline is explicit for the same reason.
		//
		// It applies to THIS call and to the read below it, the two the facade makes through a raw REST
		// client. Its object reads go through a typed client, which exposes no per-request retry knob;
		// those are bounded by a deadline on their own client instead (see AddToManager).
		MaxRetries(0).
		Timeout(coreContentCallTimeout).
		Do(ctx)

	var code int
	result.StatusCode(&code)
	raw, rawErr := result.Raw()
	if code == 0 {
		// Same zero-code case as on the read path (see relayReadResult): an error status the client could
		// not decode leaves the code only inside the error. Recover it and hand back a Status body of our
		// own carrying its reason and message — not the original bytes, which the library dropped before
		// this call saw them, but everything in them a client acts on.
		if relayed, ok := statusFromError(uploadContentSubject(contentName), rawErr); ok {
			return relayed.code, relayed.statusBody(), nil
		}
		// No HTTP response at all: there is nothing to relay, so this is the caller's error.
		return 0, nil, fmt.Errorf("%s: %w", uploadContentSubject(contentName), rawErr)
	}
	if code >= http.StatusBadRequest && !isStatusBody(raw) {
		// A failure body that is not a Kubernetes Status — a proxy in the path answering text/plain, say.
		// Relaying those bytes would hand the facade's own client a body its client library cannot decode
		// while the response claims to be JSON, and that client then lands in exactly this defect one hop
		// up: with no Status to read, it derives the reason from its own request verb, making a 409 on a
		// POST an "AlreadyExists" that means nothing here. So the code is kept and the body is restated
		// as a Status carrying the far end's own text.
		relayed := relayedCoreError(uploadContentSubject(contentName), code, raw)
		return relayed.code, relayed.statusBody(), nil
	}
	// A success body is relayed BYTE FOR BYTE and never re-encoded: it carries the reconstructed
	// checkpoint name, which is a field of the content layer's own envelope and not of a Status, so
	// decoding and re-encoding it here would silently drop the one value the client came for.
	return code, raw, nil
}

// isStatusBody reports whether a response body is a Kubernetes Status, which is what the content layer
// itself answers with and what a client of this facade can decode.
func isStatusBody(body []byte) bool {
	var probe struct {
		Kind string `json:"kind"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Kind == "Status"
}

// uploadContentSubject names what a failed forward was writing to.
func uploadContentSubject(contentName string) string {
	return fmt.Sprintf("forward the manifests of SnapshotContent %q", contentName)
}

// statusBody renders a relayed failure as the Status body a client of this facade receives, for the paths
// that relay a body rather than an error.
func (e *coreStatusError) statusBody() []byte {
	body, err := json.Marshal(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Reason:   metav1.StatusReason(e.reason),
		Message:  e.message,
		Code:     int32(e.code),
	})
	if err != nil {
		// Marshalling a Status of plain strings cannot fail; if it ever did, the code still has to reach
		// the client, so the body degrades rather than the answer.
		return []byte(`{"kind":"Status","apiVersion":"v1","status":"Failure"}`)
	}
	return body
}
