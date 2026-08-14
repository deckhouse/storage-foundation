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

package dataimport

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
	"github.com/deckhouse/storage-foundation/common/publish"
)

// corsTestReconciler builds the minimal reconciler makeIngressCfg needs: the published Ingress
// configuration depends only on the controller config, the generated names and the DataImport namespace,
// never on the API server.
func corsTestReconciler() *DataImportReconciler {
	return &DataImportReconciler{
		Config: &config.Options{
			ControllerNamespace:    "d8-storage-foundation",
			OriginIngressNamespace: "d8-system",
		},
		dataImport: &dev1alpha1.DataImport{
			ObjectMeta: metav1.ObjectMeta{Namespace: "user-ns", Name: "my-import"},
		},
		names: common.NewNames(dev1alpha1.KindPVC, "my-pvc", "user-ns", "my-import"),
	}
}

// splitHeaderList parses a cors-allow-headers / cors-expose-headers annotation value into individual
// names, canonicalized: HTTP header names are case-insensitive and so is the browser's CORS matching, so
// a membership check must not depend on the spelling a particular handler happens to use.
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

// TestImportCORSAllowHeaders asserts the ALLOW list of the upload ingress, read off the value
// makeIngressCfg actually publishes (so a constant that is defined but not wired fails here too).
//
// Why a table and not a string comparison against the constant: the value is assembled from the shared
// baseline plus the protocol headers, and the failure mode worth catching is a name going missing —
// either because someone rebuilt the list by hand and dropped a standard header, or because a new
// protocol header was added to the exporter and not here. A name missing from this list is not stripped
// from the request: the preflight answers without it, the browser fails the upload as a CORS error and
// never sends the PUT, so the exporter logs nothing.
//
// Only a browser notices, because only a browser enforces CORS — a CLI client sees every header
// regardless of these lists, even over this same ingress (`d8 data import upload --publish` uploads to
// this very status.publicURL with these very headers). So neither the exporter's tests, nor its logs, nor
// a green CLI upload can catch an incomplete list; this test is the only automatic guard. (The 400 from
// CheckRequiredHeaders is the separate case of a client that reaches the exporter and omits a header
// itself.)
//
// Limit of this check: the required-header list of the exporter lives in another Go module, so it cannot
// be imported and is restated here as an independent oracle. The two must be kept in sync by hand; both
// sides carry a comment pointing at the other.
func TestImportCORSAllowHeaders(t *testing.T) {
	allowHeaders := splitHeaderList(corsTestReconciler().makeIngressCfg().CorsAllowHeaders)
	require.NotEmpty(t, allowHeaders, "the upload ingress must publish a cors-allow-headers list")

	tests := []struct {
		header string
		why    string
	}{
		// Hard requirements of the write path: the exporter rejects a PUT without any of these with 400.
		{header: "X-Content-Length", why: "required by the exporter on every PUT"},
		{header: "X-Offset", why: "required by the exporter on every PUT"},
		{header: "X-Attribute-Permissions", why: "required by the exporter on every PUT"},
		{header: "X-Attribute-Uid", why: "required by the exporter on every PUT"},
		{header: "X-Attribute-Gid", why: "required by the exporter on every PUT"},
		// Optional per file, but carries metadata the importer cannot reconstruct.
		{header: "X-Attribute-ModTime", why: "carries the file modification time"},
		{header: "X-LinkTarget", why: "carries the symlink target"},
		// Standard names: the annotation replaces the ingress-nginx default instead of extending it, so
		// losing them here is exactly as fatal as losing a protocol header.
		{header: "Authorization", why: "the browser authenticates every request"},
		{header: "Content-Type", why: "the PUT body is typed"},
		{header: "Range", why: "part of the baseline the annotation replaces"},
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

// TestImportCORSExposeHeaders asserts the EXPOSE list of the upload ingress separately from the allow
// list, because the two fail in different ways and a single combined test would hide the worse one: a
// regression that drops X-Expected-Offset leaves every request working and only breaks recovery.
//
// Cross-origin JS reads no response header that is not exposed — an unexposed header is not hidden, it is
// indistinguishable from absent. All three names are therefore load-bearing:
//   - X-Next-Offset is where the next chunk starts, so without it a resumable upload cannot advance;
//   - X-Expected-Offset comes with the 409 raised on an offset mismatch, while the other 409 — a
//     competing writer — carries no headers at all. Without it the client cannot tell a recoverable
//     reposition from an unrecoverable conflict;
//   - X-Device-Size is the size of the target device, answered on the same HEAD as X-Next-Offset. Writing
//     past the end is refused with a bare 416 carrying no headers, so a client that cannot read the size
//     can neither reject an oversized archive up front nor explain the failure once it happens.
//
// Limit of this check: it verifies the names are published, not that the browser can read them (that is
// nginx behaviour, checked manually against a cluster).
func TestImportCORSExposeHeaders(t *testing.T) {
	exposeHeaders := splitHeaderList(corsTestReconciler().makeIngressCfg().CorsExposeHeaders)
	require.NotEmpty(t, exposeHeaders, "the upload ingress must publish a cors-expose-headers list")

	tests := []struct {
		header string
		why    string
	}{
		{header: "X-Next-Offset", why: "the offset the next chunk must start at"},
		{header: "X-Expected-Offset", why: "distinguishes an offset mismatch from a competing writer on 409"},
		{header: "X-Device-Size", why: "the size of the target device; a 416 for writing past the end carries no headers"},
	}

	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			assert.Contains(t, exposeHeaders, http.CanonicalHeaderKey(tt.header),
				"%s must be readable cross-origin: %s", tt.header, tt.why)
		})
	}

	t.Logf("checked %d expose-headers against a list of %d", len(tests), len(exposeHeaders))
}

// TestMakeIngressCfg pins the rest of the published upload contract, so the CORS work cannot quietly
// change routing, the allowed methods or the body-size limit.
func TestMakeIngressCfg(t *testing.T) {
	r := corsTestReconciler()
	cfg := r.makeIngressCfg()

	assert.Equal(t, "PUT, POST, HEAD, OPTIONS", cfg.CorsAllowMethods)
	assert.Equal(t, "64m", cfg.ProxyBodySize)
	assert.Equal(t, "/user-ns/pvc/my-pvc", cfg.Path)
	assert.Equal(t, "d8-storage-foundation", cfg.IngressName.Namespace)
	assert.Equal(t, r.names.IngressResourceName, cfg.IngressName.Name)
	assert.Equal(t, r.names.HeadlessServiceName, cfg.ServiceName.Name)
	assert.Equal(t, "d8-system", cfg.OriginIngress.Namespace)
	assert.Equal(t, common.OriginIngressName, cfg.OriginIngress.Name)
	assert.Equal(t, common.IngressSecretName, cfg.TargetSecretName)
	// The baseline is included by construction, not by hand-copying.
	assert.True(t, strings.HasPrefix(cfg.CorsAllowHeaders, publish.CORSStandardAllowHeaders+","),
		"the allow list must be built on top of the shared baseline")
}
