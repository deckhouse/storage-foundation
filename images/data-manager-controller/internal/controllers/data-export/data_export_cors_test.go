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

package dataexport

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common/publish"
)

// corsTestIngressCfg returns the Ingress configuration the download endpoint publishes. It goes through
// makePublishConfigs on purpose: a constant that is defined but never wired must fail these tests.
func corsTestIngressCfg(t *testing.T) publish.IngressCfg {
	t.Helper()
	r := createTestReconciler(nil, nil, createTestConfig())
	dataExport := &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{Namespace: dataExportNamespace, Name: dataExportName},
	}
	_, ingressCfg := r.makePublishConfigs(dataExport, testNames)
	return ingressCfg
}

// splitHeaderList parses a cors-allow-headers / cors-expose-headers annotation value into individual
// names, canonicalized: HTTP header names are case-insensitive and so is the browser's CORS matching, so
// a membership check must not depend on the spelling a particular handler happens to use (the exporter
// writes X-Attribute-Modtime here and X-Attribute-ModTime on the import side).
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

// TestExportCORSAllowHeaders checks the ALLOW list of the download ingress. A download sends nothing
// beyond the standard names, so the list is the shared baseline verbatim — but it still has to be
// published, because the annotation replaces the ingress-nginx default rather than extending it: leaving
// it empty while CORS is on strips Authorization and Range from the preflight.
//
// Limit of this check: it asserts the value the controller publishes, not nginx behaviour.
func TestExportCORSAllowHeaders(t *testing.T) {
	allowHeaders := splitHeaderList(corsTestIngressCfg(t).CorsAllowHeaders)
	require.NotEmpty(t, allowHeaders, "the download ingress must publish a cors-allow-headers list")

	tests := []struct {
		header string
		why    string
	}{
		{header: "Authorization", why: "the browser authenticates every request"},
		{header: "Range", why: "chunked reads request byte ranges"},
		{header: "Content-Type", why: "part of the baseline the annotation replaces"},
		{header: "DNT", why: "part of the baseline the annotation replaces"},
		{header: "Keep-Alive", why: "part of the baseline the annotation replaces"},
		{header: "User-Agent", why: "part of the baseline the annotation replaces"},
		{header: "X-Requested-With", why: "part of the baseline the annotation replaces"},
		{header: "If-Modified-Since", why: "part of the baseline the annotation replaces"},
		{header: "Cache-Control", why: "part of the baseline the annotation replaces"},
	}

	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			assert.Contains(t, allowHeaders, http.CanonicalHeaderKey(tt.header),
				"%s must be allowed cross-origin: %s", tt.header, tt.why)
		})
	}

	t.Logf("checked %d allow-headers against a list of %d", len(tests), len(allowHeaders))
}

// TestExportCORSExposeHeaders checks the EXPOSE list of the download ingress, kept separate from the
// allow list because the failure is silent: with the request itself unaffected, a dropped name only shows
// up as a wrong filename or an unverifiable range, deep inside a download.
//
// Cross-origin JS reads no response header that is not exposed — an unexposed header is indistinguishable
// from absent. This is not hypothetical: the existing export dialog reads Content-Disposition, which was
// never exposed, and silently fell back to a made-up filename.
//
// Limit of this check: it verifies the names are published, not that the browser can read them (nginx
// behaviour, checked manually against a cluster). X-Type is intentionally not among them — an entry's type
// reaches clients through the directory-listing JSON, not a header.
func TestExportCORSExposeHeaders(t *testing.T) {
	exposeHeaders := splitHeaderList(corsTestIngressCfg(t).CorsExposeHeaders)
	require.NotEmpty(t, exposeHeaders, "the download ingress must publish a cors-expose-headers list")

	tests := []struct {
		header string
		why    string
	}{
		{header: "Content-Disposition", why: "carries the filename of a file or of the raw block device"},
		{header: "Content-Range", why: "lets the client verify a 206 covers the requested bytes"},
		{header: "Accept-Ranges", why: "tells the client range requests are supported"},
		{header: "X-Attribute-Permissions", why: "file mode, needed to restore the entry"},
		{header: "X-Attribute-Uid", why: "file owner, needed to restore the entry"},
		{header: "X-Attribute-Gid", why: "file group, needed to restore the entry"},
		{header: "X-Attribute-Modtime", why: "modification time, needed to restore the entry"},
		{header: "X-Attribute-Hash-Md5", why: "source checksum, needed to verify content"},
		{header: "X-LinkTarget", why: "symlink target, whose body is empty by design"},
	}

	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			assert.Contains(t, exposeHeaders, http.CanonicalHeaderKey(tt.header),
				"%s must be readable cross-origin: %s", tt.header, tt.why)
		})
	}

	t.Logf("checked %d expose-headers against a list of %d", len(tests), len(exposeHeaders))
}

// TestExportPublishConfigCORSMethods pins the rest of the published download contract: the CORS work must
// not widen the allowed methods (a download endpoint accepts no writes) nor raise the body-size limit.
func TestExportPublishConfigCORSMethods(t *testing.T) {
	cfg := corsTestIngressCfg(t)

	assert.Equal(t, "GET, HEAD, OPTIONS", cfg.CorsAllowMethods)
	assert.Empty(t, cfg.ProxyBodySize, "a download endpoint has no request body to size")
	assert.Equal(t, publish.CORSStandardAllowHeaders, cfg.CorsAllowHeaders,
		"a download sends no protocol headers, so the allow list is the shared baseline")
}
