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

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deckhouse/sds-common-lib/logger"
)

// The credential material every case below hunts for in the log. Each string is unlike anything the code or
// the log format produces on its own, so finding it in the output can only mean it was copied there.
const (
	bearerTokenSecret = "bearer-token-must-not-be-logged"
	cookieSecret      = "cookie-value-must-not-be-logged"
	proxySecret       = "proxy-token-must-not-be-logged"
	customSecret      = "custom-header-must-not-be-logged"
	schemelessSecret  = "schemeless-token-must-not-be-logged"
)

// logRequestOutput runs the logging middleware over one request and returns everything it wrote.
func logRequestOutput(t *testing.T, request *http.Request) string {
	t.Helper()

	var out bytes.Buffer
	log, err := logger.NewLoggerToWriter(&out, logger.InfoLevel)
	require.NoError(t, err)

	served := false
	handler := logRequest(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusNoContent)
	}), log)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	require.True(t, served, "the middleware must pass the request on, whatever it decides to log")
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.NotEmpty(t, out.String(), "nothing was logged at all, so this test would pass over a leak too")

	return out.String()
}

// headerRecords decodes what logRequestOutput returned and indexes its "request header" records by header
// name. The assertions read fields rather than search the text because the log is JSON, which escapes the
// angle brackets of hiddenValue.
func headerRecords(t *testing.T, logged string) map[string]map[string]any {
	t.Helper()

	records := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), "every log line must be one JSON record: %s", line)
		if record["msg"] != "request header" {
			continue
		}
		name, ok := record["name"].(string)
		require.True(t, ok, "a request header record must carry the header name: %s", line)
		records[name] = record
	}

	return records
}

// TestLogRequestHidesCredentialHeaderValues is the guard on what the pod's log is allowed to contain. Pod
// logs are read by more people than a tenant's token ever was, and they outlive the transfer the token was
// issued for, so a token copied into them is a credential handed to a wider audience for longer. What stays
// in the log is the header's NAME, and for an authentication header the scheme: that a request arrived
// carrying a Bearer is what someone debugging a refusal needs, and it is not the secret.
func TestLogRequestHidesCredentialHeaderValues(t *testing.T) {
	request := httptest.NewRequest(http.MethodPut, "/api/v1/block", nil)
	request.Header.Set("Authorization", "Bearer "+bearerTokenSecret)
	request.Header.Set("Cookie", "session="+cookieSecret)
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:"+proxySecret)))
	request.Header.Set("X-Custom-Not-Known-Here", customSecret)
	request.Header.Set("X-Offset", "4096")

	logged := logRequestOutput(t, request)

	for _, secret := range []string{bearerTokenSecret, cookieSecret, proxySecret, customSecret} {
		assert.NotContains(t, logged, secret, "credential material must never reach the log, not even truncated")
	}
	// The base64 of the proxy credential, checked separately: an encoded secret is still the secret, and
	// nothing about the plaintext assertion above would notice it.
	assert.NotContains(t, logged, base64.StdEncoding.EncodeToString([]byte("user:"+proxySecret)))

	// The facts that remain, and that a person reading the log needs.
	headers := headerRecords(t, logged)
	for _, name := range []string{"Authorization", "Cookie", "X-Custom-Not-Known-Here"} {
		assert.Equal(t, hiddenValue, headers[name]["value"], "%s must be logged by name with its value hidden", name)
	}
	hidden := 0
	for _, record := range headers {
		if record["value"] == hiddenValue {
			hidden++
		}
	}
	assert.Equal(t, 4, hidden,
		"every header outside the allow-list must be logged with its value hidden: Authorization, Cookie, Proxy-Authorization and the unknown one")
	assert.Equal(t, "Bearer", headers["Authorization"]["scheme"], "the scheme of an authentication header is diagnostics, not a secret")
	assert.Equal(t, "Basic", headers["Proxy-Authorization"]["scheme"])

	// An allow-listed header keeps its value: the point is to hide credentials, not to blind the log.
	assert.Equal(t, []any{"4096"}, headers["X-Offset"]["value"], "an allow-listed upload header must still be logged with its value")
}

// TestLogRequestReportsUnrecognizedAuthorizationScheme covers the shape that would turn the scheme field
// itself into the leak: a value with no scheme word, or one this server does not know. Its first word is not
// echoed — a bare token pasted into the header would otherwise be printed as if it were a scheme name.
func TestLogRequestReportsUnrecognizedAuthorizationScheme(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "a bare token with no scheme word", header: schemelessSecret},
		{name: "a scheme this server does not know", header: schemelessSecret + " " + schemelessSecret},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/api/v1/block", nil)
			request.Header.Set("Authorization", test.header)

			logged := logRequestOutput(t, request)

			assert.NotContains(t, logged, schemelessSecret)
			authorization := headerRecords(t, logged)["Authorization"]
			assert.Equal(t, hiddenValue, authorization["value"])
			assert.Equal(t, unrecognizedScheme, authorization["scheme"])
		})
	}
}

// TestAuthorizationHeaderIsNotLoggable is the guard on the allow-list itself: no name that carries a
// credential may appear in it. The check is a walk over the list rather than a reading of it, because the
// list is where a future one-line edit would do the damage.
func TestAuthorizationHeaderIsNotLoggable(t *testing.T) {
	credentialHeaders := []string{
		"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
		"X-Api-Key", "X-Auth-Token", "X-Amz-Security-Token",
	}

	for _, name := range credentialHeaders {
		assert.False(t, loggableHeaders[http.CanonicalHeaderKey(name)],
			"%s carries credentials and must not be on the list of headers logged with their value", name)
	}

	t.Logf("%d credential header names checked against an allow-list of %d", len(credentialHeaders), len(loggableHeaders))
	require.NotEmpty(t, loggableHeaders, "an empty allow-list would make the checks above vacuous")
}
