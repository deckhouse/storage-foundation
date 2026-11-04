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

package main

import (
	"fmt"
	"net/http"

	"github.com/deckhouse/sds-common-lib/fs"
	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/authorization"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/export_block"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/export_filesystem"
	importblock "github.com/deckhouse/storage-foundation/images/data-exporter/internal/import_block"
	importfilesystem "github.com/deckhouse/storage-foundation/images/data-exporter/internal/import_filesystem"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/middleware"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/repository"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/ttl_control"
)

// router is the http.ServeMux surface this package needs: register a pattern, serve a request. It is an
// interface, and not *http.ServeMux, because net/http cannot report which patterns were registered on a
// ServeMux — it answers "who would serve this request", never "what is registered". The registration call is
// therefore the only place where the complete route set of this server is observable, and the guard
// TestEveryRegisteredRouteRefusesAnonymousRequest observes it by passing an implementation that records every
// pattern it is handed.
//
// Register routes through muxAddHandler and nothing else. A pattern handed straight to the mux is invisible
// to that guard, so the route it adds is never checked again — and an unchecked route is how the end-of-upload
// endpoint came to be served with no authorization at all, from a registration function of its own that
// nothing tied to the rest. TestRoutesAreRegisteredOnlyThroughOnePoint keeps the sources honest about it.
type router interface {
	http.Handler
	Handle(pattern string, handler http.Handler)
}

// NewDataServerHandler assembles everything this server answers. Every route is registered on the router
// first, and then the ROUTER AS A WHOLE — not each route on its own — is wrapped in the middleware chain a
// request has to pass. Authorization is therefore not a property a route can be registered without: every
// path this server answers, the ones registered here and any added later, is answered only after the chain
// has let the request through, and a path with no route behind it answers 401 rather than 404.
//
// It used to be the other way round — the chain wrapped the data handler and each route was registered with
// an already-wrapped handler — and a route that came with a registration function of its own escaped the
// chain completely: no authorization, no required-header check, and outside the transfer accounting, with
// nothing at the registration site to say so. Guards: TestEveryRegisteredRouteRefusesAnonymousRequest,
// TestPathWithNoRouteRefusesAnonymousRequest.
//
// accounting is the transfer accounting that keeps the idle window open, and the order of the chain below is
// where it belongs rather than an accident: authorization outermost, accounting immediately inside it, the
// rest inside that. So a request refused for its credentials is invisible to the window — otherwise one
// anonymous request per idle period, from anywhere that reaches the published endpoint, would hold the
// window open for as long as it kept sending — while everything a recognised caller does, refusals from the
// handlers included, counts. Guard: TestRefusalOfAnUnrecognisedCallerIsNotTransferActivity.
func NewDataServerHandler(
	mux router,
	cfg *config.Config,
	dataHandler http.Handler,
	finishedHandler http.Handler,
	authorizer authorization.UserAuthorizer,
	accounting func(http.Handler) http.Handler,
	logger logger.Logger,
) (http.Handler, error) {
	switch cfg.Mode {
	case config.VolumeModeBlock:
		MuxAddBlockHandler(mux, cfg.URLOpt, dataHandler, logger)
	case config.VolumeModeFilesystem:
		MuxAddFSHandler(mux, cfg.URLOpt, dataHandler, logger)
	default:
		return nil, fmt.Errorf("invalid program argument, mode %v", cfg.Mode)
	}

	middlewares := []func(http.Handler) http.Handler{
		func(next http.Handler) http.Handler {
			return authorization.Authorize(next, authorizer, cfg.Operation, cfg.URLOpt.DataManagerNamespace)
		},
		accounting,
	}

	// Both of these belong to the upload direction only: an export has nothing to finish, and the required
	// header set describes a write.
	if cfg.Operation == common.OperationImport {
		MuxAddFinishedHandler(mux, cfg.URLOpt, finishedHandler, logger)
		middlewares = append(middlewares, middleware.CheckRequiredHeaders)
	}

	return middleware.Chain(mux, middlewares...), nil
}

func NewExporterHandler(fsys fs.OS, path string, mode config.VolumeMode, logger logger.Logger) (http.Handler, error) {
	switch mode {
	case config.VolumeModeBlock:
		return export_block.NewBlockExporter(fsys, path, logger)
	case config.VolumeModeFilesystem:
		return export_filesystem.NewFilesystemExporter(fsys, path, logger)
	default:
		return nil, fmt.Errorf("invalid program argument, mode %v", mode)
	}
}

func NewImportHandler(fsys fs.OS, path string, mode config.VolumeMode, logger logger.Logger, client *repository.Client, cfg *config.Config, repository *repository.Client) (http.Handler, error) {
	switch mode {
	case config.VolumeModeBlock:
		return importblock.NewImportBlockHandler(fsys, path, logger, client, cfg, repository)
	case config.VolumeModeFilesystem:
		return importfilesystem.NewImportFilesystemHandler(fsys, path, logger, client, cfg, repository)
	default:
		return nil, fmt.Errorf("invalid program argument, mode %v", mode)
	}
}

// Add handlers for filesystem export mode
func MuxAddFSHandler(mux router, opt config.URLOpt, handler http.Handler, logger logger.Logger) {
	muxAddHandler(mux, "files/", opt, handler, logger)
	muxAddHandler(mux, "files", opt, http.HandlerFunc(export_filesystem.WrongPrefixFilesInformer), logger)
	muxAddHandler(mux, "block", opt, http.HandlerFunc(export_filesystem.WrongPrefixBlockInformer), logger)
	muxAddHandler(mux, "block/", opt, http.HandlerFunc(export_filesystem.WrongPrefixBlockInformer), logger)
}

// Add handlers for block device export mode
func MuxAddBlockHandler(mux router, opt config.URLOpt, handler http.Handler, logger logger.Logger) {
	muxAddHandler(mux, "block", opt, handler, logger)
	muxAddHandler(mux, "block/", opt, http.HandlerFunc(export_block.WrongPrefixBlockInformer), logger)
	muxAddHandler(mux, "files", opt, http.HandlerFunc(export_block.WrongPrefixFilesInformer), logger)
	muxAddHandler(mux, "files/", opt, http.HandlerFunc(export_block.WrongPrefixFilesInformer), logger)
}

func muxAddHandler(mux router, suffix string, opt config.URLOpt, handler http.Handler, logger logger.Logger) {
	shortPrefix := fmt.Sprintf("/api/v1/%s", suffix)
	// GetPathPrefix already ends with "/", so the suffix is appended WITHOUT a leading slash. With one, the
	// long pattern gets a "//" (".../<name>//api/v1/<suffix>") that http.ServeMux never matches against a
	// cleaned single-slash request: a call through the published long URL 404s while the short path keeps
	// working, which reads as a routing mystery rather than a typo.
	longPrefix := fmt.Sprintf("%sapi/v1/%s", GetPathPrefix(opt), suffix)
	logger.Info(
		"Registering HTTP handler",
		"shortPathPrefix", shortPrefix,
		"servedPathPrefix", GetPathPrefix(opt),
		"longPathPrefix", longPrefix,
	)
	mux.Handle(longPrefix, http.StripPrefix(longPrefix, handler))
	mux.Handle(shortPrefix, http.StripPrefix(shortPrefix, handler))
}

func GetPathPrefix(opt config.URLOpt) string {
	return fmt.Sprintf("/%s/%s/%s/", opt.DataManagerNamespace, opt.DataManagerTargetKindShort, opt.DataManagerTargetName)
}

// NewFinishedHandler serves the end-of-upload signal: a POST flips status.serverState to Finished, which is
// the gate the controller waits for before rebinding the target and reporting the import complete. The last
// data PUT does not finalise on its own, so an upload that is never completed this way stays open until the
// idle window expires.
//
// This route is inside the transfer accounting (ttl_control.RequestNotifier) like every other route, and
// that is deliberate even though completion is the signal that the idle window should close. The accounting
// counts BYTES MOVED, not requests: a successful completion reads no request body and answers 204 with no
// body, so it contributes nothing and cannot postpone the idle deadline. What does count is the body of an
// error answer to a caller that got this far, meaning an authorized one, and that is wanted — a client
// retrying a failed completion is activity, and closing the window while a retry is in flight is exactly the
// wrong moment. The other side of the same coin: because the accounting is byte-based, a completion cannot
// refresh status.accessTimestamp either, so the observable trace of this call is the serverState transition
// it performs and not the timestamp.
func NewFinishedHandler(stateSetter ttl_control.DataManagerServerStateSetter, cfg *config.Config, logger logger.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := stateSetter.SetServerState(r.Context(), common.OperationImport, cfg.URLOpt.DataManagerNamespace, cfg.DataManagerName, common.ServerStateFinished); err != nil {
			logger.Error("failed to set data import finished", "error", err)
			http.Error(w, "failed to set data import finished", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// Register the unified end-of-upload endpoint (short and long prefixes)
func MuxAddFinishedHandler(mux router, opt config.URLOpt, handler http.Handler, logger logger.Logger) {
	muxAddHandler(mux, "finished", opt, handler, logger)
}
