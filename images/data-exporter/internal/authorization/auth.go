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

package authorization

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

func Authorize(next http.Handler, client UserAuthorizer, operation common.Operation, namespace string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authData, err := GetAuthDataFromRequest(r)
		if err != nil {
			// A missing/empty/malformed credential is a client authentication failure (401), not a server
			// error (500). In particular a request that reaches the exporter with no Authorization header and
			// no TLS peer certificate — the normal case for the publish path, where nginx terminates TLS so a
			// client cert never arrives at the pod — must be rejected as Unauthorized rather than surfaced as
			// an internal error.
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}

		authenticated, username, groups, err := AuthenticateUser(r.Context(), client, *authData)
		if err != nil {
			http.Error(w, "failed to authenticate user: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if !authenticated {
			http.Error(w, "Bearer token not authorized", http.StatusUnauthorized)
			return
		}

		allowed, reason, err := client.AuthorizeUser(r.Context(), operation, namespace, username, groups)
		if err != nil {
			http.Error(w, "failed to authorize user: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if !allowed {
			msg := fmt.Sprintf("Not enough permissions for dataExport, reason: %s", reason)
			http.Error(w, msg, http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func GetAuthDataFromRequest(r *http.Request) (*dev1alpha1.AuthData, error) {
	authData := &dev1alpha1.AuthData{}

	// Get Bearer token from Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		if strings.HasPrefix(authHeader, "Bearer ") {
			authData.AuthType = dev1alpha1.AuthTypeBearer
			authData.Token = strings.TrimPrefix(authHeader, "Bearer ")
			if authData.Token == "" {
				return nil, fmt.Errorf("bearer token is empty")
			}
		} else if strings.HasPrefix(authHeader, "Basic ") {
			authData.AuthType = dev1alpha1.AuthTypeBasic
			// Basic auth is not used in this context, but we can extract username and password if needed
			payload := strings.TrimPrefix(authHeader, "Basic ")
			credentials, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return nil, fmt.Errorf("failed to decode basic auth credentials: %w", err)
			}
			parts := strings.SplitN(string(credentials), ":", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid basic auth credentials format")
			}
			authData.Username = parts[0]
			authData.Password = parts[1]
		}
	}

	// Get client certificate from TLS connection state
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		authData.AuthType = dev1alpha1.AuthTypeCert
		authData.ClientCert = *r.TLS.PeerCertificates[0]
	}

	// If no auth type is set, return an error
	if authData.AuthType == "" {
		return nil, fmt.Errorf("no valid authentication method found")
	}

	return authData, nil
}

func AuthenticateUser(ctx context.Context, client UserAuthorizer, authData dev1alpha1.AuthData) (authenticated bool, username string, groups []string, err error) {
	// authenticated = false

	switch authData.AuthType {
	case dev1alpha1.AuthTypeBearer:
		return client.AuthenticateUserByToken(ctx, authData.Token)
	case dev1alpha1.AuthTypeBasic:
		return false, "", nil, fmt.Errorf("basic auth is not supported")
	case dev1alpha1.AuthTypeCert:
		return AuthenticateUserByCert(authData.ClientCert)
	default:
		return false, "", nil, fmt.Errorf("unsupported authentication method")
	}
}

func AuthenticateUserByCert(cert x509.Certificate) (authenticated bool, username string, groups []string, err error) {
	// Authenticated true because TLS connection is established and certificate is verified by the server
	authenticated = true
	username = cert.Subject.CommonName
	groups = cert.Subject.Organization

	// No need to create TokenReview for certificate authentication
	return authenticated, username, groups, nil
}
