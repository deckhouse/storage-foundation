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

package middleware

import (
	"encoding/json"
	"net/http"
)

// CheckRequiredHeaders rejects a PUT that does not carry the full set of upload-protocol headers, so a
// handler never has to reason about a half-described write. Other methods pass through untouched: the
// requirement belongs to the write path only, and HEAD in particular is how a client asks for the
// current resume point.
//
// The list is mirrored (as an ingress CORS allow-list) by the DataImport controller in
// images/data-manager-controller/internal/controllers/data-import. A header required here but not
// allowed there breaks browser uploads WITHOUT producing a 400 anywhere: the preflight answers without
// the name, the browser fails the request as a CORS error and never sends the PUT, so this middleware is
// never reached and the exporter logs nothing at all. Do not look for the rejection in these logs — and
// note that a CLI upload through the same ingress still succeeds, since CORS is enforced by browsers
// only, so it cannot be used to confirm the allow-list is complete.
func CheckRequiredHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			// The return is essential, not cosmetic: without it a non-PUT fell through into the check
			// below after having already been served, and the response was written twice — either the
			// error JSON appended to the payload already sent (net/http drops the second WriteHeader with
			// "superfluous response.WriteHeader", but not the body), or, when the headers happened to be
			// present, the whole handler run a second time.
			next.ServeHTTP(w, r)
			return
		}

		requiredHeaders := []string{"X-Content-Length", "X-Offset", "X-Attribute-Permissions", "X-Attribute-Uid", "X-Attribute-Gid"}
		missingHeaders := []string{}

		for _, header := range requiredHeaders {
			h := r.Header.Get(header)
			if h == "" {
				missingHeaders = append(missingHeaders, header)
			}
		}

		if len(missingHeaders) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			response := map[string]interface{}{
				"error":          "Missing required headers",
				"missingHeaders": missingHeaders,
			}
			_ = json.NewEncoder(w).Encode(response)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func Chain(handler http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	wrapped := handler
	for i := len(middlewares) - 1; i >= 0; i-- {
		wrapped = middlewares[i](wrapped)
	}
	return wrapped
}
