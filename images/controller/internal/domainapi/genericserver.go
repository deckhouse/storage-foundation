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
	"fmt"
	"net"
	"net/http"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	utilcompatibility "k8s.io/apiserver/pkg/util/compatibility"
	openapihandler "k8s.io/kube-openapi/pkg/handler"
	openapihandler3 "k8s.io/kube-openapi/pkg/handler3"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// buildAggregatedAPIServer assembles a genericapiserver that fronts the subresource routes with the
// standard aggregation-layer handler chain:
//
//   - delegated authentication: the kube-apiserver front-proxy client certificate is validated against
//     the cluster-managed requestheader CA and allowed-names, in union with bearer-token authentication
//     via TokenReview;
//   - delegated authorization: every request is decided by SubjectAccessReview against the attributes
//     genericapiserver derives from the URL (verb, group, resource, subresource, namespace), so end-user
//     and inter-controller calls are gated by the same RBAC. Health probes are exempt by default.
//
// The delegate carries every route this server owns and is mounted as the NotFoundHandler of the
// non-go-restful mux, so it is reached only AFTER those filters have run.
//
// Both halves of authn/authz reach the kube-apiserver through the pod's in-cluster credentials; no
// kubeconfig is involved.
func buildAggregatedAPIServer(name, addr, certFile, keyFile string, delegate http.Handler) (*genericapiserver.GenericAPIServer, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("serving cert and key files are required for the aggregated apiserver")
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("parse api address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("parse api port %q: %w", portStr, err)
	}

	// A minimal scheme suffices: no typed REST storage is registered, but genericapiserver needs codecs
	// that can serialize metav1.Status for the errors it produces itself.
	scheme := runtime.NewScheme()
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	codecs := serializer.NewCodecFactory(scheme)

	config := genericapiserver.NewConfig(codecs)
	config.EffectiveVersion = utilcompatibility.DefaultBuildEffectiveVersion()

	// WithLoopback is required: genericapiserver.New() rejects a config whose LoopbackClientConfig is nil.
	// Only the loopback variant's ApplyTo generates the self-signed loopback certificate and fills that
	// field in; the plain secure-serving ApplyTo leaves it nil even when our own cert and key are given.
	secure := genericoptions.NewSecureServingOptions().WithLoopback()
	secure.BindPort = port
	if host != "" {
		secure.BindAddress = net.ParseIP(host)
	}
	secure.ServerCert.CertKey.CertFile = certFile
	secure.ServerCert.CertKey.KeyFile = keyFile
	if err := secure.ApplyTo(&config.SecureServing, &config.LoopbackClientConfig); err != nil {
		return nil, fmt.Errorf("apply secure serving: %w", err)
	}

	authn := genericoptions.NewDelegatingAuthenticationOptions()
	if err := authn.ApplyTo(&config.Authentication, config.SecureServing, config.OpenAPIConfig); err != nil {
		return nil, fmt.Errorf("apply delegated authentication: %w", err)
	}

	authz := genericoptions.NewDelegatingAuthorizationOptions()
	if err := authz.ApplyTo(&config.Authorization); err != nil {
		return nil, fmt.Errorf("apply delegated authorization: %w", err)
	}

	return newAggregatedServer(config.Complete(nil), name, delegate)
}

// newAggregatedServer turns a completed config into the running server: the endpoints it owns itself, the
// OpenAPI documents, and the delegate behind everything else.
//
// It is split from the config assembly above because that half cannot run outside a cluster — delegated
// authn/authz read a ConfigMap in kube-system and secure serving binds a port — while this half is pure
// wiring and is what the tests drive.
func newAggregatedServer(completed genericapiserver.CompletedConfig, name string, delegate http.Handler) (*genericapiserver.GenericAPIServer, error) {
	server, err := completed.New(name, genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, fmt.Errorf("build generic apiserver: %w", err)
	}

	if err := installOpenAPIRoutes(server, name); err != nil {
		return nil, fmt.Errorf("install openapi routes: %w", err)
	}

	// genericapiserver owns /healthz, /readyz, /livez, /version, /apis discovery and the OpenAPI documents
	// installed just above. Our routes are not typed API groups, so the group discovery and the
	// subresources fall through to this delegate.
	server.Handler.NonGoRestfulMux.NotFoundHandler(delegate)

	return server, nil
}

// installOpenAPIRoutes makes the server answer /openapi/v2 and /openapi/v3.
//
// The kube-apiserver OpenAPI aggregation controllers poll both paths for every APIService. Without these
// routes the requests fall through to the delegate mux, which has no such route and answers 404, and the
// control plane then logs a failed download and requeues for as long as the APIService exists — forever,
// once per APIService, and this module registers two. The upstream branch meant to skip a 404 quietly
// cannot fire: the sentinel is wrapped in a message before the caller compares against it. What ends the
// retry loop is the 200; the content of the document does not matter to it.
//
// The documents are assembled here rather than through the generic OpenAPI config on purpose. That path
// builds the spec from the registered go-restful routes and needs a generated definition for every model
// they mention, starting with the version type behind /version. With no generated definitions in this
// module it does not degrade to an empty document — it aborts the process during PrepareRun, so the
// controller never starts at all.
func installOpenAPIRoutes(server *genericapiserver.GenericAPIServer, name string) error {
	v2 := openapihandler.NewOpenAPIService(&spec.Swagger{
		SwaggerProps: spec.SwaggerProps{
			Swagger: "2.0",
			Info: &spec.Info{
				InfoProps: spec.InfoProps{
					Title: name,
					// Nothing injects a build version into this binary, so anything derived from the build
					// would report the version of the Kubernetes libraries as if it were the module's.
					// This is the same placeholder the upstream config completion falls back to, and the
					// aggregator ignores the Info of a downloaded document anyway.
					Version: "unversioned",
				},
			},
			// Empty, and not a placeholder: the subresources are connector-style routes on the delegate
			// mux, not typed REST storage, so there is no generated schema to publish. Of a downloaded
			// document the aggregator keeps only the paths under that APIService's own group and version,
			// so an empty one contributes nothing to the cluster-wide spec.
			Paths: &spec.Paths{Paths: map[string]spec.PathItem{}},
		},
	})
	v2.RegisterOpenAPIVersionedService("/openapi/v2", server.Handler.NonGoRestfulMux)

	// No group version is registered, so the v3 discovery document is an empty path map. That is a valid
	// answer: it says this server publishes no v3 schema, instead of a 404 the aggregator reads as a
	// failed download.
	v3 := openapihandler3.NewOpenAPIService()
	if err := v3.RegisterOpenAPIV3VersionedService("/openapi/v3", server.Handler.NonGoRestfulMux); err != nil {
		// The current library always returns nil here, but propagating it keeps a future bump from
		// starting the server without /openapi/v3 and reintroducing the retry loop silently.
		return fmt.Errorf("register /openapi/v3: %w", err)
	}

	return nil
}
