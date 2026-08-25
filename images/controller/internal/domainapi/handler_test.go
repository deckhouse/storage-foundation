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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readyFacade() *Handler {
	return newTestHandler(
		newFakeReader().withVolumeSnapshot(newVolumeSnapshot(withImportMode())).withContent(newSnapshotContent()),
		&fakeCore{
			manifests:  capturedPVCJSON(),
			uploadCode: http.StatusOK,
			uploadBody: []byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`),
		},
	)
}

// The alias exists so a client released on another schedule keeps working. That is only true if the two
// groups answer identically: a difference in the payload means the alias is a second, diverging API
// rather than the same one under an old name.
//
// Each case pins the status it expects as well. Comparing the two groups against each other alone is not
// enough: a request that starts failing on BOTH groups still compares equal, and the case then quietly
// stops comparing the payload it was written for and compares two identical error bodies instead.
func TestAliasGroup_AnswersByteIdenticallyToThePrimaryGroup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantStatus int
		call       func(group string) *http.Request
	}{
		{
			name:       SubresourceManifestsDownload,
			wantStatus: http.StatusOK,
			call: func(group string) *http.Request {
				return httptest.NewRequest(http.MethodGet,
					subresourcePath(group, testNamespace, testVSName, SubresourceManifestsDownload), nil)
			},
		},
		{
			name:       SubresourceManifestsRestore,
			wantStatus: http.StatusOK,
			call: func(group string) *http.Request {
				return httptest.NewRequest(http.MethodGet,
					subresourcePath(group, testNamespace, testVSName, SubresourceManifestsRestore), nil)
			},
		},
		{
			name:       SubresourceManifestsUpload,
			wantStatus: http.StatusOK,
			call: func(group string) *http.Request {
				return uploadRequest(group, fmt.Sprintf(`{"manifests":%s,"childRefs":[]}`, uploadManifests))
			},
		},
		{
			name:       "a failing request",
			wantStatus: http.StatusNotFound,
			call: func(group string) *http.Request {
				return httptest.NewRequest(http.MethodGet,
					subresourcePath(group, testNamespace, "absent", SubresourceManifestsDownload), nil)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := serve(readyFacade(), tc.call(PrimaryGroup))
			alias := serve(readyFacade(), tc.call(DeprecatedAliasGroup))

			if primary.Code != tc.wantStatus {
				t.Fatalf("primary group answered %d, want %d — this case no longer compares what it was written for (body %s)",
					primary.Code, tc.wantStatus, primary.Body.String())
			}
			if primary.Code != alias.Code {
				t.Fatalf("status differs between groups: primary %d, alias %d", primary.Code, alias.Code)
			}
			if !bytes.Equal(primary.Body.Bytes(), alias.Body.Bytes()) {
				t.Fatalf("body differs between groups:\nprimary %s\nalias   %s", primary.Body.String(), alias.Body.String())
			}
		})
	}
}

// A client must be able to see that it is on the deprecated path, and be told where to go. The primary
// group must NOT carry the warning, or the header stops meaning anything.
func TestAliasGroup_WarnsAndThePrimaryGroupDoesNot(t *testing.T) {
	for _, path := range []string{
		"/apis/%s",
		"/apis/%s/" + GroupVersion,
	} {
		aliasRec := serve(readyFacade(), httptest.NewRequest(http.MethodGet, fmt.Sprintf(path, DeprecatedAliasGroup), nil))
		primaryRec := serve(readyFacade(), httptest.NewRequest(http.MethodGet, fmt.Sprintf(path, PrimaryGroup), nil))

		warning := aliasRec.Header().Get("Warning")
		if warning == "" {
			t.Fatalf("%s: the deprecated group answered without a Warning header", path)
		}
		if !strings.Contains(warning, PrimaryGroup) {
			t.Fatalf("%s: the Warning does not name the group to move to: %q", path, warning)
		}
		if !strings.HasPrefix(warning, "299 ") {
			t.Fatalf("%s: the Warning is not in the form clients surface (299 - \"...\"): %q", path, warning)
		}
		if got := primaryRec.Header().Get("Warning"); got != "" {
			t.Fatalf("%s: the primary group must not warn, got %q", path, got)
		}
	}
}

// A Warning is also owed on the subresources themselves and on failures, not only on discovery — a
// client that never reads discovery would otherwise never learn it is on the old path.
func TestAliasGroup_WarnsOnSubresourcesAndOnErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{
			name: "successful subresource",
			req: httptest.NewRequest(http.MethodGet,
				subresourcePath(DeprecatedAliasGroup, testNamespace, testVSName, SubresourceManifestsDownload), nil),
		},
		{
			name: "failing subresource",
			req: httptest.NewRequest(http.MethodGet,
				subresourcePath(DeprecatedAliasGroup, testNamespace, "absent", SubresourceManifestsDownload), nil),
		},
		{
			name: "unknown subresource",
			req: httptest.NewRequest(http.MethodGet,
				subresourcePath(DeprecatedAliasGroup, testNamespace, testVSName, "manifests-nonesuch"), nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(readyFacade(), tc.req)
			if rec.Header().Get("Warning") == "" {
				t.Fatalf("no Warning header on the deprecated group (status %d)", rec.Code)
			}
		})
	}
}

// Discovery must report the group that was ADDRESSED. Reporting the primary group on the alias would
// make a client build its next request against a group its own cached discovery never mentioned.
func TestDiscovery_ReportsTheAddressedGroup(t *testing.T) {
	for _, group := range []string{PrimaryGroup, DeprecatedAliasGroup} {
		t.Run(group, func(t *testing.T) {
			rec := serve(readyFacade(), httptest.NewRequest(http.MethodGet, "/apis/"+group, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("group discovery status: got %d want 200", rec.Code)
			}
			var apiGroup metav1.APIGroup
			if err := json.Unmarshal(rec.Body.Bytes(), &apiGroup); err != nil {
				t.Fatalf("group discovery is not an APIGroup: %v (body %s)", err, rec.Body.String())
			}
			if apiGroup.Name != group {
				t.Fatalf("APIGroup.name: got %q want %q", apiGroup.Name, group)
			}
			want := group + "/" + GroupVersion
			if apiGroup.PreferredVersion.GroupVersion != want {
				t.Fatalf("preferredVersion.groupVersion: got %q want %q", apiGroup.PreferredVersion.GroupVersion, want)
			}

			rec = serve(readyFacade(), httptest.NewRequest(http.MethodGet, "/apis/"+group+"/"+GroupVersion, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("version discovery status: got %d want 200", rec.Code)
			}
			var list metav1.APIResourceList
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatalf("version discovery is not an APIResourceList: %v (body %s)", err, rec.Body.String())
			}
			if list.GroupVersion != want {
				t.Fatalf("APIResourceList.groupVersion: got %q want %q", list.GroupVersion, want)
			}

			advertised := map[string][]string{}
			for _, resource := range list.APIResources {
				if resource.Kind != KindVolumeSnapshot {
					t.Fatalf("resource %q reports kind %q, want %q", resource.Name, resource.Kind, KindVolumeSnapshot)
				}
				if !resource.Namespaced {
					t.Fatalf("resource %q is advertised as cluster-scoped", resource.Name)
				}
				advertised[resource.Name] = resource.Verbs
			}
			for name, verbs := range map[string][]string{
				ResourceVolumeSnapshots + "/" + SubresourceManifestsDownload: {"get"},
				ResourceVolumeSnapshots + "/" + SubresourceManifestsRestore:  {"get"},
				ResourceVolumeSnapshots + "/" + SubresourceManifestsUpload:   {"create"},
			} {
				got, ok := advertised[name]
				if !ok {
					t.Fatalf("discovery does not advertise %q (has %v)", name, advertised)
				}
				if strings.Join(got, ",") != strings.Join(verbs, ",") {
					t.Fatalf("verbs of %q: got %v want %v", name, got, verbs)
				}
			}
			// The bare resource has no storage here, so advertising it would promise a read that cannot
			// be served.
			if _, ok := advertised[ResourceVolumeSnapshots]; ok {
				t.Fatalf("discovery advertises the virtual resource itself, which has no storage: %v", advertised)
			}
		})
	}
}

// The resource is virtual: it exists only as a hook for the subresources. Every address that would be a
// read of the resource itself has to be a NotFound rather than an empty success.
func TestVirtualResource_HasNothingToReadWithoutASubresource(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "object without a subresource", path: fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s", PrimaryGroup, GroupVersion, testNamespace, ResourceVolumeSnapshots, testVSName)},
		{name: "collection", path: fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s", PrimaryGroup, GroupVersion, testNamespace, ResourceVolumeSnapshots)},
		{name: "cluster-scoped collection", path: fmt.Sprintf("/apis/%s/%s/%s", PrimaryGroup, GroupVersion, ResourceVolumeSnapshots)},
		{name: "another resource", path: fmt.Sprintf("/apis/%s/%s/namespaces/%s/persistentvolumeclaims/%s/%s", PrimaryGroup, GroupVersion, testNamespace, testVSName, SubresourceManifestsDownload)},
		{name: "unknown subresource", path: subresourcePath(PrimaryGroup, testNamespace, testVSName, "manifests-nonesuch")},
		{name: "unknown version", path: fmt.Sprintf("/apis/%s/v9/namespaces/%s/%s/%s/%s", PrimaryGroup, testNamespace, ResourceVolumeSnapshots, testVSName, SubresourceManifestsDownload)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(readyFacade(), httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status: got %d want 404 (body %s)", rec.Code, rec.Body.String())
			}
			if got := decodeStatus(t, rec.Body.Bytes()).Reason; got != "NotFound" {
				t.Fatalf("reason: got %q want NotFound", got)
			}
		})
	}
}

// Each subresource accepts exactly one method — the one discovery advertises a verb for. Accepting more
// would let a client mutate through a route RBAC evaluated as a read.
func TestSubresources_AcceptOnlyTheAdvertisedMethod(t *testing.T) {
	for _, tc := range []struct {
		subresource string
		wrongMethod string
	}{
		{subresource: SubresourceManifestsDownload, wrongMethod: http.MethodPost},
		{subresource: SubresourceManifestsRestore, wrongMethod: http.MethodPost},
		{subresource: SubresourceManifestsUpload, wrongMethod: http.MethodGet},
	} {
		t.Run(tc.subresource, func(t *testing.T) {
			rec := serve(readyFacade(), httptest.NewRequest(tc.wrongMethod,
				subresourcePath(PrimaryGroup, testNamespace, testVSName, tc.subresource), nil))

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status: got %d want 405 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// The namespace and name are spliced into a request path built for another server, so they are checked
// against the naming rules before they get there.
func TestSubresources_RejectMalformedNamespaceAndName(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{
			name: "namespace is not a DNS label",
			path: subresourcePath(PrimaryGroup, "Not_A_Namespace", testVSName, SubresourceManifestsDownload),
		},
		{
			name: "name is not a DNS subdomain",
			path: subresourcePath(PrimaryGroup, testNamespace, "Not_A_Name", SubresourceManifestsDownload),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(readyFacade(), httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// Path syntax in the address must never end in a served read. Where exactly it is stopped differs — an
// encoded separator becomes extra segments the dispatcher does not recognise, a literal ".." is
// normalised by the router before the dispatcher sees it at all — so the assertion is on the outcome
// that matters: no manifests come back.
func TestSubresources_NeverServeAnAddressCarryingPathSyntax(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{
			name: "encoded separator in the namespace",
			path: fmt.Sprintf("/apis/%s/%s/namespaces/..%%2Fkube-system/%s/%s/%s",
				PrimaryGroup, GroupVersion, ResourceVolumeSnapshots, testVSName, SubresourceManifestsDownload),
		},
		{
			name: "parent directory as the namespace",
			path: subresourcePath(PrimaryGroup, "..", testVSName, SubresourceManifestsDownload),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(readyFacade(), httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code == http.StatusOK {
				t.Fatalf("the address was served: %s", rec.Body.String())
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("PersistentVolumeClaim")) {
				t.Fatalf("manifests leaked through a traversal address: %s", rec.Body.String())
			}
		})
	}
}

// Health endpoints belong to genericapiserver, which never forwards them here. A route registered for
// them on this mux would be dead code that looks exactly like a live probe target.
func TestHandler_DoesNotRegisterHealthRoutes(t *testing.T) {
	mux := http.NewServeMux()
	readyFacade().SetupRoutes(mux)

	for _, path := range []string{"/healthz", "/readyz", "/livez", "/version"} {
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern != "" {
			t.Fatalf("%s is handled by the delegate mux (pattern %q); genericapiserver owns it", path, pattern)
		}
	}
}
