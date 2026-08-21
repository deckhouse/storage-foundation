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
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericapiserver "k8s.io/apiserver/pkg/server"
	utilcompatibility "k8s.io/apiserver/pkg/util/compatibility"
	"k8s.io/client-go/rest"
)

// openAPITestServer builds the server the way production does, minus the half that cannot run outside a
// cluster: delegated authn/authz read a ConfigMap in kube-system, and secure serving binds a port. What
// is exercised is newAggregatedServer itself, so an installOpenAPIRoutes call lost in a refactor fails
// these tests instead of shipping.
//
// delegateCalled reports whether a request fell through to the route mux, which is how the failure being
// guarded against manifests: the mux has no /openapi route and answers 404, and the control plane then
// retries and logs for every APIService, forever.
func openAPITestServer(t *testing.T) (server *genericapiserver.GenericAPIServer, delegateCalled *bool) {
	t.Helper()

	called := false
	delegate := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNotFound)
	})

	scheme := runtime.NewScheme()
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	codecs := serializer.NewCodecFactory(scheme)

	config := genericapiserver.NewConfig(codecs)
	config.EffectiveVersion = utilcompatibility.DefaultBuildEffectiveVersion()
	// Stand-ins for what secure serving provides in production: Complete() derives the external address
	// from the bound port, and New() rejects a nil loopback config.
	config.ExternalAddress = "127.0.0.1:8443"
	config.LoopbackClientConfig = &rest.Config{Host: "https://127.0.0.1:8443"}

	built, err := newAggregatedServer(config.Complete(nil), apiServerName, delegate)
	if err != nil {
		t.Fatalf("build the aggregated apiserver: %v", err)
	}
	// Production calls PrepareRun before serving; it starts no listener.
	built.PrepareRun()

	return built, &called
}

func openAPIGet(server *genericapiserver.GenericAPIServer, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, req)
	return rec
}

func TestOpenAPI_V2IsServedByTheApiserverAndNotTheDelegate(t *testing.T) {
	server, delegateCalled := openAPITestServer(t)

	// The aggregation controller asks for JSON explicitly.
	rec := openAPIGet(server, "/openapi/v2", map[string]string{"Accept": "application/json"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if *delegateCalled {
		t.Fatal("/openapi/v2 fell through to the route mux; the aggregation controller would see a 404 and retry forever")
	}

	var doc struct {
		Swagger string `json:"swagger"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	if doc.Swagger != "2.0" {
		t.Fatalf("swagger: got %q want 2.0", doc.Swagger)
	}
	if doc.Info.Title != apiServerName {
		t.Fatalf("info.title: got %q want %q", doc.Info.Title, apiServerName)
	}
	if doc.Info.Version == "" {
		t.Fatal("info.version is empty; the document must carry one")
	}
	// The document describes no path on purpose: the subresources are routes on the delegate mux, not
	// typed REST storage. A path appearing here would be published cluster-wide with nothing behind it.
	if len(doc.Paths) != 0 {
		t.Fatalf("info: the document declares %d paths, want none", len(doc.Paths))
	}
}

func TestOpenAPI_V3IsServedByTheApiserverAndNotTheDelegate(t *testing.T) {
	server, delegateCalled := openAPITestServer(t)

	rec := openAPIGet(server, "/openapi/v3", map[string]string{"Accept": "application/json"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if *delegateCalled {
		t.Fatal("/openapi/v3 fell through to the route mux; the aggregation controller would see a 404 and retry forever")
	}
	var doc struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the discovery document is not JSON: %v", err)
	}
	if len(doc.Paths) != 0 {
		t.Fatalf("the v3 discovery document declares %d group versions, want none", len(doc.Paths))
	}
}

// The aggregation controller does not fetch the document the way a browser would: it negotiates. Serving
// only plain JSON was enough to stop the retry loop, but a client that asks for protobuf, compression or
// a conditional refetch has to get a valid answer too, and none of that is code of ours — it is the
// library's, which is exactly why a change of library or of how it is wired can drop it unnoticed.
func TestOpenAPI_V2NegotiatesContent(t *testing.T) {
	server, _ := openAPITestServer(t)

	// Both spellings of the protobuf media type are accepted, and both are answered with the canonical
	// one. Clients in the field still send the older spelling with the "@" separator, so dropping it
	// would break them while the newer one kept working.
	const canonicalProtobuf = "application/com.github.proto-openapi.spec.v2.v1.0+protobuf"
	for _, accept := range []string{
		"application/com.github.proto-openapi.spec.v2@v1.0+protobuf",
		canonicalProtobuf,
	} {
		t.Run("protobuf via "+accept, func(t *testing.T) {
			rec := openAPIGet(server, "/openapi/v2", map[string]string{"Accept": accept})
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != canonicalProtobuf {
				t.Fatalf("content-type: got %q want %q", got, canonicalProtobuf)
			}
			if rec.Body.Len() == 0 {
				t.Fatal("the protobuf document is empty")
			}
			if json.Valid(rec.Body.Bytes()) {
				t.Fatal("the protobuf response is JSON; content negotiation was ignored")
			}
		})
	}

	t.Run("a client asking for gzip gets a readable document", func(t *testing.T) {
		rec := openAPIGet(server, "/openapi/v2", map[string]string{
			"Accept":          "application/json",
			"Accept-Encoding": "gzip",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d want 200", rec.Code)
		}

		body := rec.Body.Bytes()
		// Compression is the serving library's, and it applies a minimum size below which it sends the
		// body as-is. This document is deliberately empty and falls under that minimum, so BOTH outcomes
		// are correct here and the assertion is on what the client ends up with, not on which branch ran.
		// Pinning Content-Encoding: gzip would be a test of the library's threshold, and would start
		// failing the day the document grows or the threshold moves.
		if rec.Header().Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("the response announces gzip but is not gzip: %v", err)
			}
			if body, err = io.ReadAll(zr); err != nil {
				t.Fatalf("read the gzip body: %v", err)
			}
		}
		if !json.Valid(body) {
			t.Fatalf("the document a gzip-capable client receives is not JSON: %s", body)
		}
	})

	t.Run("conditional refetch", func(t *testing.T) {
		first := openAPIGet(server, "/openapi/v2", map[string]string{"Accept": "application/json"})
		etag := first.Header().Get("Etag")
		if etag == "" {
			t.Fatal("no ETag on the document; a client cannot refetch conditionally")
		}

		second := openAPIGet(server, "/openapi/v2", map[string]string{
			"Accept":        "application/json",
			"If-None-Match": etag,
		})
		if second.Code != http.StatusNotModified {
			t.Fatalf("conditional refetch: got %d want 304", second.Code)
		}
		if second.Body.Len() != 0 {
			t.Fatalf("a 304 must carry no body, got %d bytes", second.Body.Len())
		}
	})
}

// Everything genericapiserver does not own itself must still reach the routes.
func TestAggregatedServer_RoutesEverythingElseToTheDelegate(t *testing.T) {
	server, delegateCalled := openAPITestServer(t)

	rec := openAPIGet(server, "/apis/"+PrimaryGroup+"/"+GroupVersion, map[string]string{"Accept": "application/json"})

	if !*delegateCalled {
		t.Fatal("the group path did not reach the delegate; the subresources would be unreachable")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status from the stub delegate: got %d want 404", rec.Code)
	}
}
