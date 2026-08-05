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

package publish

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	testPublicDomain = "example.com"
	testHost         = "api.example.com"
	testPath         = "/user-ns/pvc/my-pvc"
	testIngressClass = "nginx"
)

func testIngressCfg() IngressCfg {
	return IngressCfg{
		IngressName:      types.NamespacedName{Namespace: "d8-storage-foundation", Name: "ingress-for-pvc-abc"},
		ServiceName:      types.NamespacedName{Namespace: "d8-storage-foundation", Name: "service-for-pvc-abc"},
		OriginIngress:    types.NamespacedName{Namespace: "d8-system", Name: "origin"},
		TargetSecretName: "ingress-secret",
		Path:             testPath,
	}
}

func makeTestIngress(t *testing.T, cfg IngressCfg) *networkingv1.Ingress {
	t.Helper()
	pathType := networkingv1.PathTypeImplementationSpecific
	return makeIngress(cfg, testHost, cfg.Path, &pathType, testIngressClass, testPublicDomain)
}

// splitHeaderList parses a cors-allow-headers / cors-expose-headers annotation value into individual
// names, canonicalized: HTTP header names are case-insensitive and so is the browser's CORS matching,
// so a membership check must not depend on the spelling the handler happens to use (the exporter writes
// both X-Attribute-ModTime and X-Attribute-Modtime).
func splitHeaderList(list string) []string {
	parts := strings.Split(list, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		names = append(names, http.CanonicalHeaderKey(part))
	}
	return names
}

// TestMakeIngressCORSAnnotations pins the CORS annotation set makeIngress emits. The expectation is the
// WHOLE annotation map, not a subset, so both directions are covered: a header list that fails to reach
// the Ingress and an annotation that appears when the corresponding IngressCfg field is empty.
//
// Limit of this check: it asserts what the controller writes onto the Ingress object. Whether
// ingress-nginx then answers a real preflight with these values requires a cluster and is covered by
// the manual acceptance step (watch the browser Network tab), not here.
func TestMakeIngressCORSAnnotations(t *testing.T) {
	tests := []struct {
		name                string
		mutate              func(cfg *IngressCfg)
		expectedAnnotations map[string]string
	}{
		{
			// The default: no CORS at all. Guards against CORS leaking onto ingresses that never asked
			// for it just because the header lists gained defaults.
			name:   "no CORS requested",
			mutate: func(_ *IngressCfg) {},
			expectedAnnotations: map[string]string{
				AnnotationBackendProtocol: "HTTPS",
			},
		},
		{
			// The pre-existing behaviour (allow-methods only), kept as a regression anchor: enabling CORS
			// must still produce exactly enable-cors + allow-methods + allow-origin, and the origin must
			// stay narrowed to the console host rather than widening to "*".
			name: "CORS without header lists",
			mutate: func(cfg *IngressCfg) {
				cfg.CorsAllowMethods = "GET, HEAD, OPTIONS"
			},
			expectedAnnotations: map[string]string{
				AnnotationBackendProtocol:  "HTTPS",
				AnnotationEnableCORS:       "true",
				AnnotationCORSAllowMethods: "GET, HEAD, OPTIONS",
				AnnotationCORSAllowOrigin:  "https://console.example.com",
			},
		},
		{
			name: "CORS with both header lists",
			mutate: func(cfg *IngressCfg) {
				cfg.CorsAllowMethods = "GET, HEAD, OPTIONS"
				cfg.CorsAllowHeaders = "Authorization,Range"
				cfg.CorsExposeHeaders = "Content-Disposition"
			},
			expectedAnnotations: map[string]string{
				AnnotationBackendProtocol:   "HTTPS",
				AnnotationEnableCORS:        "true",
				AnnotationCORSAllowMethods:  "GET, HEAD, OPTIONS",
				AnnotationCORSAllowOrigin:   "https://console.example.com",
				AnnotationCORSAllowHeaders:  "Authorization,Range",
				AnnotationCORSExposeHeaders: "Content-Disposition",
			},
		},
		{
			// Each list is independent: exposing response headers on a download endpoint must not force
			// an allow-list, and vice versa.
			name: "CORS with allow list only",
			mutate: func(cfg *IngressCfg) {
				cfg.CorsAllowMethods = "PUT, POST, HEAD, OPTIONS"
				cfg.CorsAllowHeaders = "Authorization,X-Offset"
			},
			expectedAnnotations: map[string]string{
				AnnotationBackendProtocol:  "HTTPS",
				AnnotationEnableCORS:       "true",
				AnnotationCORSAllowMethods: "PUT, POST, HEAD, OPTIONS",
				AnnotationCORSAllowOrigin:  "https://console.example.com",
				AnnotationCORSAllowHeaders: "Authorization,X-Offset",
			},
		},
		{
			name: "CORS with expose list only",
			mutate: func(cfg *IngressCfg) {
				cfg.CorsAllowMethods = "GET, HEAD, OPTIONS"
				cfg.CorsExposeHeaders = "X-Next-Offset"
			},
			expectedAnnotations: map[string]string{
				AnnotationBackendProtocol:   "HTTPS",
				AnnotationEnableCORS:        "true",
				AnnotationCORSAllowMethods:  "GET, HEAD, OPTIONS",
				AnnotationCORSAllowOrigin:   "https://console.example.com",
				AnnotationCORSExposeHeaders: "X-Next-Offset",
			},
		},
		{
			// Documents the gating: allow-methods is what turns CORS on, so header lists alone produce
			// nothing. nginx ignores cors-* annotations without enable-cors, and writing them anyway
			// would advertise a CORS setup that does not exist.
			name: "header lists without allow-methods produce no CORS annotations",
			mutate: func(cfg *IngressCfg) {
				cfg.CorsAllowHeaders = "Authorization,X-Offset"
				cfg.CorsExposeHeaders = "X-Next-Offset"
			},
			expectedAnnotations: map[string]string{
				AnnotationBackendProtocol: "HTTPS",
			},
		},
		{
			// The upload ingress shape: CORS lists coexist with the raised body-size limit.
			name: "CORS with header lists and proxy body size",
			mutate: func(cfg *IngressCfg) {
				cfg.CorsAllowMethods = "PUT, POST, HEAD, OPTIONS"
				cfg.CorsAllowHeaders = "Authorization,X-Offset"
				cfg.CorsExposeHeaders = "X-Next-Offset,X-Expected-Offset"
				cfg.ProxyBodySize = "64m"
			},
			expectedAnnotations: map[string]string{
				AnnotationBackendProtocol:   "HTTPS",
				AnnotationEnableCORS:        "true",
				AnnotationCORSAllowMethods:  "PUT, POST, HEAD, OPTIONS",
				AnnotationCORSAllowOrigin:   "https://console.example.com",
				AnnotationCORSAllowHeaders:  "Authorization,X-Offset",
				AnnotationCORSExposeHeaders: "X-Next-Offset,X-Expected-Offset",
				AnnotationProxyBodySize:     "64m",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testIngressCfg()
			tt.mutate(&cfg)

			ing := makeTestIngress(t, cfg)

			assert.Equal(t, tt.expectedAnnotations, ing.Annotations)
		})
	}

	t.Logf("checked %d makeIngress configurations", len(tests))
}

// TestMakeIngressRoutingUnaffectedByCORS guards the rest of the Ingress against the CORS change: the
// annotations are the only thing the header lists may touch, routing and TLS must stay identical.
func TestMakeIngressRoutingUnaffectedByCORS(t *testing.T) {
	plain := testIngressCfg()
	plain.CorsAllowMethods = "GET, HEAD, OPTIONS"

	withHeaders := plain
	withHeaders.CorsAllowHeaders = CORSStandardAllowHeaders
	withHeaders.CorsExposeHeaders = "Content-Disposition"

	plainIng := makeTestIngress(t, plain)
	headersIng := makeTestIngress(t, withHeaders)

	assert.Equal(t, plainIng.Spec, headersIng.Spec)
	assert.Equal(t, plainIng.Name, headersIng.Name)
	assert.Equal(t, plainIng.Namespace, headersIng.Namespace)
}

// TestCORSStandardAllowHeaders pins the baseline list. It exists because the annotation REPLACES the
// ingress-nginx default: dropping a name here silently revokes a header for every endpoint at once, and
// Authorization or Content-Type going missing breaks the browser client on the preflight.
func TestCORSStandardAllowHeaders(t *testing.T) {
	required := []string{
		"DNT",
		"Keep-Alive",
		"User-Agent",
		"X-Requested-With",
		"If-Modified-Since",
		"Cache-Control",
		"Content-Type",
		"Range",
		"Authorization",
	}

	for _, header := range required {
		t.Run(header, func(t *testing.T) {
			assert.Contains(t, splitHeaderList(CORSStandardAllowHeaders), http.CanonicalHeaderKey(header),
				"the ingress annotation replaces the controller default, so %q must be restated explicitly", header)
		})
	}

	t.Logf("checked %d standard allow-headers", len(required))
}
