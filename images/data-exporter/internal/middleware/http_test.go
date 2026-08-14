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

package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// handlerPayload is what the wrapped handler answers with. It is checked byte-for-byte so that a second
// write appended after it (the bug this test was written for) shows up as a body mismatch rather than
// being swallowed.
const handlerPayload = "handler payload"

// allRequiredHeaders is the full set CheckRequiredHeaders demands on a PUT. It is restated here instead of
// being read from the implementation: a test that derived the set from the code under test would pass on
// any set at all, including an accidentally emptied one.
var allRequiredHeaders = map[string]string{
	"X-Content-Length":        "1024",
	"X-Offset":                "0",
	"X-Attribute-Permissions": "0644",
	"X-Attribute-Uid":         "0",
	"X-Attribute-Gid":         "0",
}

// countingHandler answers with handlerPayload and records how many times it was invoked, which is how the
// double-serve case is detected.
type countingHandler struct {
	calls int
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls++
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(handlerPayload))
}

func headersExcept(exclude ...string) map[string]string {
	excluded := make(map[string]bool, len(exclude))
	for _, name := range exclude {
		excluded[name] = true
	}
	headers := make(map[string]string, len(allRequiredHeaders))
	for name, value := range allRequiredHeaders {
		if !excluded[name] {
			headers[name] = value
		}
	}
	return headers
}

// TestCheckRequiredHeadersNonPutPassesThrough covers the methods the middleware must not touch. HEAD
// matters most: it is how a client asks for the current resume offset, and it carries none of the
// upload-protocol headers by definition.
//
// The assertions are deliberately about the RESPONSE AS A WHOLE, not just its status: the defect being
// guarded against was a missing return, after which a non-PUT was served by the handler and then fell
// through into the check, appending the error JSON to a response that had already been sent (net/http
// drops the second WriteHeader but not the body) — or, when the headers happened to be present, running
// the handler a second time. A status-only assertion passes in both cases.
//
// Limit of this check: it covers the methods the exporter actually serves plus a couple of neighbours; the
// middleware makes no distinction between them beyond "not PUT", so enumerating the whole HTTP method
// space would add nothing.
func TestCheckRequiredHeadersNonPutPassesThrough(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		headers map[string]string
	}{
		{name: "HEAD without protocol headers", method: http.MethodHead},
		{name: "GET without protocol headers", method: http.MethodGet},
		{name: "OPTIONS without protocol headers", method: http.MethodOptions},
		{name: "POST without protocol headers", method: http.MethodPost},
		// A non-PUT that happens to carry the headers must not be treated differently: with the missing
		// return this was the double-serve case.
		{name: "GET with protocol headers", method: http.MethodGet, headers: allRequiredHeaders},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &countingHandler{}
			req := httptest.NewRequest(tt.method, "/api/v1/block", nil)
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()

			CheckRequiredHeaders(handler).ServeHTTP(rec, req)

			assert.Equal(t, 1, handler.calls, "the wrapped handler must be invoked exactly once")
			assert.Equal(t, http.StatusOK, rec.Code, "the status must come from the handler, not the middleware")
			assert.Equal(t, handlerPayload, rec.Body.String(), "nothing may be appended to the handler response")
			assert.NotContains(t, rec.Body.String(), "Missing required headers")
		})
	}

	t.Logf("checked %d non-PUT requests", len(tests))
}

// TestCheckRequiredHeadersPutRejectsMissing asserts that an incompletely described write is refused before
// the handler runs, and that the answer names every missing header (that list is what a client, browser or
// d8, has to act on).
func TestCheckRequiredHeadersPutRejectsMissing(t *testing.T) {
	tests := []struct {
		name            string
		headers         map[string]string
		expectedMissing []string
	}{
		{
			name:    "no headers at all",
			headers: nil,
			expectedMissing: []string{
				"X-Content-Length", "X-Offset", "X-Attribute-Permissions", "X-Attribute-Uid", "X-Attribute-Gid",
			},
		},
		{
			name:            "offset missing",
			headers:         headersExcept("X-Offset"),
			expectedMissing: []string{"X-Offset"},
		},
		{
			name:            "content length missing",
			headers:         headersExcept("X-Content-Length"),
			expectedMissing: []string{"X-Content-Length"},
		},
		{
			name:            "attributes missing",
			headers:         headersExcept("X-Attribute-Permissions", "X-Attribute-Uid", "X-Attribute-Gid"),
			expectedMissing: []string{"X-Attribute-Permissions", "X-Attribute-Uid", "X-Attribute-Gid"},
		},
		{
			// An empty value is as unusable as an absent header, so it must be reported the same way.
			name:            "offset present but empty",
			headers:         withHeader(headersExcept("X-Offset"), "X-Offset", ""),
			expectedMissing: []string{"X-Offset"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &countingHandler{}
			req := httptest.NewRequest(http.MethodPut, "/api/v1/block", nil)
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()

			CheckRequiredHeaders(handler).ServeHTTP(rec, req)

			assert.Equal(t, 0, handler.calls, "an incomplete PUT must not reach the handler")
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

			var body struct {
				Error          string   `json:"error"`
				MissingHeaders []string `json:"missingHeaders"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "Missing required headers", body.Error)
			assert.ElementsMatch(t, tt.expectedMissing, body.MissingHeaders)
		})
	}

	t.Logf("checked %d incomplete PUT requests", len(tests))
}

// TestCheckRequiredHeadersPutWithAllHeadersPassesThrough is the positive PUT case: a fully described write
// reaches the handler untouched, exactly once.
func TestCheckRequiredHeadersPutWithAllHeadersPassesThrough(t *testing.T) {
	handler := &countingHandler{}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/block", nil)
	for name, value := range allRequiredHeaders {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()

	CheckRequiredHeaders(handler).ServeHTTP(rec, req)

	assert.Equal(t, 1, handler.calls)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, handlerPayload, rec.Body.String())
}

// TestChainOrder pins the wrapping order of Chain: the middlewares run left to right around the handler,
// which is what lets the authorization middleware reject a request before CheckRequiredHeaders reports the
// missing headers of a request that was never allowed in the first place.
func TestChainOrder(t *testing.T) {
	var order []string
	record := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	handler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		order = append(order, "handler")
	})

	Chain(handler, record("first"), record("second")).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, []string{"first", "second", "handler"}, order)
}

func withHeader(headers map[string]string, name, value string) map[string]string {
	headers[name] = value
	return headers
}
