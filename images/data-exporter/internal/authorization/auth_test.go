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

package authorization_test

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/authorization"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/mock"
)

const (
	testNamespace = "test-ns"
	testUsername  = "test-user"
	testToken     = "test-token"
)

var testGroups = []string{"test-group"}

// servedMarker answers 204 and reports whether it ran at all. Every refusal below asserts on both: the
// status the caller sees, and the fact that the wrapped handler never got the request. The second is the
// one that matters — a refusal that still lets the handler run has already done the damage, whatever
// status ends up on the wire.
func servedMarker() (handler http.Handler, served *bool) {
	ran := false

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusNoContent)
	}), &ran
}

func TestAuthorizePassesAuthenticatedAndPermittedRequest(t *testing.T) {
	ctrl := gomock.NewController(t)
	client := mock.NewMockUserAuthorizer(ctrl)
	client.EXPECT().AuthenticateUserByToken(gomock.Any(), testToken).Return(true, testUsername, testGroups, nil)
	client.EXPECT().
		AuthorizeUser(gomock.Any(), common.OperationExport, testNamespace, testUsername, testGroups).
		Return(true, "", nil)

	next, served := servedMarker()
	handler := authorization.Authorize(next, client, common.OperationExport, testNamespace)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/block", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	assert.True(t, *served, "a request with credentials the authorizer accepts must reach the handler")
	assert.Equal(t, http.StatusNoContent, recorder.Code)
}

// TestAuthorizeRefusesRequestWithoutCredentials covers the shape every request through the published ingress
// has: nginx terminates TLS, so no client certificate reaches the pod, and an unnamed caller sends no
// Authorization header either. Nothing about such a request is asked of the API server — it is refused
// where it arrives.
func TestAuthorizeRefusesRequestWithoutCredentials(t *testing.T) {
	ctrl := gomock.NewController(t)
	// Without a single EXPECT: a call to the authorizer here means credentials were invented somewhere.
	client := mock.NewMockUserAuthorizer(ctrl)

	next, served := servedMarker()
	handler := authorization.Authorize(next, client, common.OperationExport, testNamespace)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/block", nil))

	assert.False(t, *served, "a request without credentials must not reach the handler")
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func certificateState(commonName string, organizations []string) *tls.ConnectionState {
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{
			Subject: pkix.Name{CommonName: commonName, Organization: organizations},
		}},
	}
}

// TestAuthorizeRefusesInvalidCredentials pins down WHICH refusal each kind of bad credential gets, because
// the status is the only thing the caller has to go on: 401 says "these credentials are not accepted, try
// others", 403 says "you are known and this is not yours, retrying is pointless", and 500 says the server
// could not decide. Getting them confused sends a client into a retry loop over something no retry fixes,
// or hides a broken authenticator behind what reads as a permission problem.
func TestAuthorizeRefusesInvalidCredentials(t *testing.T) {
	tests := []struct {
		name       string
		authHeader string
		tlsState   *tls.ConnectionState
		expect     func(*mock.MockUserAuthorizer)
		wantStatus int
		wantBody   string
	}{
		{
			name:       "bearer token the API server does not accept",
			authHeader: "Bearer " + testToken,
			expect: func(client *mock.MockUserAuthorizer) {
				client.EXPECT().AuthenticateUserByToken(gomock.Any(), testToken).Return(false, "", nil, nil)
			},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "Bearer token not authorized",
		},
		{
			name:       "empty bearer token, refused without asking the API server",
			authHeader: "Bearer ",
			wantStatus: http.StatusUnauthorized,
			wantBody:   "bearer token is empty",
		},
		{
			name:       "authorization header in a scheme this server does not know",
			authHeader: "Token " + testToken,
			wantStatus: http.StatusUnauthorized,
			wantBody:   "no valid authentication method found",
		},
		{
			name:       "basic credentials that are not valid base64",
			authHeader: "Basic not-base64!!",
			wantStatus: http.StatusUnauthorized,
			wantBody:   "failed to decode basic auth credentials",
		},
		{
			// Basic auth is well-formed here and still refused, because this server supports bearer tokens
			// and client certificates only. What this case pins is that the request is refused and never
			// reaches a handler; the status it is refused with, 500, describes the situation worse than a
			// 401 would — the client sent a scheme that is not supported, which is not a server failure.
			// Changing it is a separate decision, so the current answer is recorded rather than asserted as
			// correct.
			name:       "well-formed basic credentials, a scheme this server does not support",
			authHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("user:password")),
			wantStatus: http.StatusInternalServerError,
			wantBody:   "basic auth is not supported",
		},
		{
			name:       "the authenticator itself fails",
			authHeader: "Bearer " + testToken,
			expect: func(client *mock.MockUserAuthorizer) {
				client.EXPECT().
					AuthenticateUserByToken(gomock.Any(), testToken).
					Return(false, "", nil, errors.New("token review failed"))
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   "failed to authenticate user",
		},
		{
			name:       "authenticated user without permission for this operation",
			authHeader: "Bearer " + testToken,
			expect: func(client *mock.MockUserAuthorizer) {
				client.EXPECT().AuthenticateUserByToken(gomock.Any(), testToken).Return(true, testUsername, testGroups, nil)
				client.EXPECT().
					AuthorizeUser(gomock.Any(), common.OperationExport, testNamespace, testUsername, testGroups).
					Return(false, "no rule grants it", nil)
			},
			wantStatus: http.StatusForbidden,
			wantBody:   "no rule grants it",
		},
		{
			// The in-cluster path: TLS verified the chain, so the certificate subject is taken as the
			// identity without a token review, and the permission check still decides.
			name:     "client certificate whose subject has no permission",
			tlsState: certificateState("cert-user", []string{"cert-group"}),
			expect: func(client *mock.MockUserAuthorizer) {
				client.EXPECT().
					AuthorizeUser(gomock.Any(), common.OperationExport, testNamespace, "cert-user", []string{"cert-group"}).
					Return(false, "no rule grants it", nil)
			},
			wantStatus: http.StatusForbidden,
			wantBody:   "no rule grants it",
		},
		{
			name:       "the permission check itself fails",
			authHeader: "Bearer " + testToken,
			expect: func(client *mock.MockUserAuthorizer) {
				client.EXPECT().AuthenticateUserByToken(gomock.Any(), testToken).Return(true, testUsername, testGroups, nil)
				client.EXPECT().
					AuthorizeUser(gomock.Any(), common.OperationExport, testNamespace, testUsername, testGroups).
					Return(false, "", errors.New("subject access review failed"))
			},
			wantStatus: http.StatusInternalServerError,
			wantBody:   "failed to authorize user",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			client := mock.NewMockUserAuthorizer(ctrl)
			if test.expect != nil {
				test.expect(client)
			}

			next, served := servedMarker()
			handler := authorization.Authorize(next, client, common.OperationExport, testNamespace)

			request := httptest.NewRequest(http.MethodGet, "/api/v1/block", nil)
			if test.authHeader != "" {
				request.Header.Set("Authorization", test.authHeader)
			}
			request.TLS = test.tlsState

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			assert.False(t, *served, "a refused request must not reach the handler")
			assert.Equal(t, test.wantStatus, recorder.Code)
			require.NotEmpty(t, test.wantBody, "every case must state what the refusal says, or it checks only the number")
			assert.Contains(t, recorder.Body.String(), test.wantBody)
		})
	}
}
