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

package authorization_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/authorization"
)

// tokenAuthorizer authenticates any bearer token and authorizes every request, so that a test can
// isolate the status code the middleware picks for a given credential.
type tokenAuthorizer struct {
	authErr error
}

func (a tokenAuthorizer) AuthenticateUserByToken(context.Context, string) (bool, string, []string, error) {
	if a.authErr != nil {
		return false, "", nil, a.authErr
	}
	return true, "user", []string{"group"}, nil
}

func (tokenAuthorizer) AuthorizeUser(context.Context, common.Operation, string, string, []string) (bool, string, error) {
	return true, "", nil
}

// The status a rejected credential gets is the point of these cases: a credential the exporter cannot
// authenticate with is the client's problem (401), while a failure while authenticating a supported
// credential is the server's (500). Reporting the former as a 500 makes a misconfigured client look
// like a broken exporter, and burns error budget on requests that were never going to succeed.
func TestAuthorizeStatusPerCredential(t *testing.T) {
	tests := []struct {
		name       string
		authHeader string
		authErr    error
		wantStatus int
	}{
		{
			name:       "bearer token is authenticated",
			authHeader: "Bearer token-123",
			wantStatus: http.StatusOK,
		},
		{
			name:       "basic auth is unsupported, not an internal error",
			authHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass")),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "unknown scheme is rejected",
			authHeader: "Negotiate abcdef",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing credential is rejected",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "empty bearer token is rejected",
			authHeader: "Bearer ",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "failure while authenticating a supported credential stays a server error",
			authHeader: "Bearer token-123",
			authErr:    errors.New("tokenreview call failed"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			served := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true
				w.WriteHeader(http.StatusOK)
			})

			handler := authorization.Authorize(
				next,
				tokenAuthorizer{authErr: tt.authErr},
				common.OperationExport,
				"de-ns",
			)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/files/file", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("status: want %d, got %d, body %q", tt.wantStatus, rr.Code, rr.Body.String())
			}

			if wantServed := tt.wantStatus == http.StatusOK; served != wantServed {
				t.Errorf("handler served: want %v, got %v", wantServed, served)
			}
		})
	}
}

func TestAuthenticateUserUnsupportedMethodIsSentinel(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/file", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")))

	authData, err := authorization.GetAuthDataFromRequest(req)
	if err != nil {
		t.Fatalf("GetAuthDataFromRequest: %v", err)
	}

	_, _, _, err = authorization.AuthenticateUser(context.Background(), tokenAuthorizer{}, *authData)
	if !errors.Is(err, authorization.ErrUnsupportedAuthMethod) {
		t.Fatalf("want ErrUnsupportedAuthMethod, got %v", err)
	}
}
