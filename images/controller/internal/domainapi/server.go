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

// Package domainapi hosts this module's aggregated API server: the three connector subresources of the
// CSI VolumeSnapshot this module owns — manifests-download, manifests-with-data-restoration and
// manifests-and-children-refs-upload.
//
// The module owns the kind's planning controller, and the API surface follows the controller: a snapshot
// tree that reaches a VolumeSnapshot is not compiled by the snapshot core but delegated here, and the
// core splices the already apply-ready answer. What this server adds on top of the core's content layer
// is the namespaced addressing of a VolumeSnapshot, the anti-spoofing handshake that ties a snapshot to
// the content it was actually bound to, and — for restore — the shared compilation that binds a restored
// PVC to the snapshot holding its data.
//
// It reads no ManifestCheckpoint itself: manifest bytes are fetched from, and written to, the snapshot
// core's cluster-scoped content layer, addressed by the resolved SnapshotContent name.
package domainapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/controller/pkg/config"
)

// apiServerName identifies this server in genericapiserver's own logging and as the title of the OpenAPI
// documents it publishes.
const apiServerName = "storage-foundation-aggregated-apiserver"

// Server runs the aggregated API server as part of the controller manager.
type Server struct {
	addr        string
	tlsCertFile string
	tlsKeyFile  string
	handler     http.Handler
	log         logr.Logger
}

// AddToManager wires the aggregated API server into the manager.
//
// The reader it serves from is cache-less on purpose. Bind state and readiness decide whether a request
// may be answered at all, and an informer's view of them lags: a stale "not bound yet" refuses a request
// that has become serviceable, and a stale "ready" hands out the manifests of a node that is being
// recaptured. A cache would also mean watching every VolumeSnapshot and SnapshotContent in the cluster to
// serve occasional requests.
func AddToManager(mgr ctrl.Manager, cfg *config.Options, log logr.Logger) error {
	// SnapshotContent belongs to the snapshot core's API group, which the manager's own scheme does not
	// register; the VolumeSnapshot is read unstructured (see loadVolumeSnapshotIdentity), which needs no
	// registration at all.
	facadeScheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(facadeScheme); err != nil {
		return fmt.Errorf("register SnapshotContent into the aggregated API server scheme: %w", err)
	}

	reader, err := client.New(facadeReaderConfig(mgr.GetConfig()), client.Options{
		Scheme: facadeScheme,
		Mapper: mgr.GetRESTMapper(),
	})
	if err != nil {
		return fmt.Errorf("build the aggregated API server's uncached reader: %w", err)
	}

	core, err := NewCoreContentClient(mgr.GetConfig())
	if err != nil {
		return err
	}

	server := NewServer(
		cfg.AggregationAPIAddr,
		cfg.AggregationAPITLSCertFile,
		cfg.AggregationAPITLSKeyFile,
		NewRestoreService(reader, core, log),
		NewUploadFacade(reader, core, log),
		log,
	)
	return mgr.Add(server)
}

// facadeReaderConfig derives the config for the facade's OWN object reads from the manager's.
//
// It gives them a deadline. client-go retries a request up to ten times on 429 and 5xx while honouring
// Retry-After, and a typed client exposes no per-request retry knob — so these reads keep the retries and
// get a wall-clock bound instead, which is what actually protects the caller's budget.
//
// It works on a COPY, and that is the load-bearing part: the manager's config is shared with every
// controller in this process, and a timeout set on it would apply to their watches too — turning a
// long-lived watch into one that expires every few seconds. Guarded by
// TestFacadeReaderConfig_BoundsTheReadsWithoutTouchingTheSharedConfig.
func facadeReaderConfig(base *rest.Config) *rest.Config {
	cfg := rest.CopyConfig(base)
	cfg.Timeout = kubeObjectReadTimeout
	return cfg
}

// NewServer assembles the route handlers. Authentication and authorization are added by genericapiserver
// at Start, so this stays cheap and unit-testable: it binds no port and reads nothing from the cluster.
func NewServer(addr, tlsCertFile, tlsKeyFile string, restore *RestoreService, upload *UploadFacade, log logr.Logger) *Server {
	mux := http.NewServeMux()
	NewHandler(restore, upload, log).SetupRoutes(mux)

	return &Server{
		addr:        addr,
		tlsCertFile: tlsCertFile,
		tlsKeyFile:  tlsKeyFile,
		handler:     mux,
		log:         log,
	}
}

// NeedLeaderElection reports false: every replica must serve.
//
// The Service in front of this server load-balances across all ready pods, so a replica that did not win
// the leader election would still receive aggregated requests — and answer them with a closed port. This
// is the opposite of the reconcilers in the same process, which must run on the leader alone.
func (s *Server) NeedLeaderElection() bool { return false }

// Start builds the genericapiserver around the routes and serves until ctx is cancelled. It blocks; on
// cancellation genericapiserver shuts down gracefully.
func (s *Server) Start(ctx context.Context) error {
	s.log.Info("starting the aggregated API server", "addr", s.addr,
		"groups", []string{PrimaryGroup, DeprecatedAliasGroup})

	genericServer, err := buildAggregatedAPIServer(apiServerName, s.addr, s.tlsCertFile, s.tlsKeyFile, s.handler)
	if err != nil {
		return err
	}
	return genericServer.PrepareRun().RunWithContext(ctx)
}
