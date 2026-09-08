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
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	iofs "io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/mock"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/ttl_control"
)

// anonymousRoutes lists the registered patterns that are allowed to answer without authorization, each with
// the reason. It is empty, and empty is the intended state: every route of this server touches the data or
// the state of one tenant's transfer, so there is nothing here that is safe to serve to an unnamed caller.
//
// An entry takes a path out from under the guard below, which is a check removed rather than a check
// relaxed. So an entry has to name what makes the path safe to expose — no tenant data in the response, no
// side effect on the transfer — and it has to go as soon as that stops being true. To keep an entry from
// outliving its reason, the guard also fails on an entry that matches no registered pattern.
var anonymousRoutes = map[string]string{}

// probeMethods is what every route is probed with. It covers at least one method each handler of this server
// accepts (an export answers GET and HEAD, an upload answers PUT, POST and HEAD, the end-of-upload route
// answers POST) plus DELETE, which no handler here accepts.
//
// The unsupported method is the sharper probe, and the reason it works is that authorization is middleware:
// it runs BEFORE any handler looks at the method. So an anonymous request must come back as 401 whatever it
// asks for, and a 405 is proof that the request reached a handler — that is, that the chain did not run —
// while also telling an unnamed caller which methods the route supports.
//
// This list needs no maintenance when a handler learns a new method: the invariant is about the order of the
// middleware and the handler, not about the set of methods.
var probeMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodDelete,
}

// routesPerSuffix is how many patterns one route suffix produces: the short path and the published long path
// (muxAddHandler registers both).
const routesPerSuffix = 2

// recordingRouter registers on a real ServeMux and remembers every pattern it was handed. It is how the
// guards below learn which routes the server has: net/http cannot report the patterns of a ServeMux, and a
// list of paths retyped into the test would stay green on the very thing that has to fail — a route added
// without authorization.
type recordingRouter struct {
	*http.ServeMux
	patterns []string
}

func (r *recordingRouter) Handle(pattern string, handler http.Handler) {
	r.patterns = append(r.patterns, pattern)
	r.ServeMux.Handle(pattern, handler)
}

// serverConfiguration is one shape of this server. One image serves both operations and both volume modes,
// and the route set differs between them: the end-of-upload route exists for an import only, and the
// required-header check joins the chain for an import only. A guard that assembled just one configuration
// would check fewer routes than the server has and stay green about it.
type serverConfiguration struct {
	name      string
	operation common.Operation
	mode      config.VolumeMode
	// minRoutes is a floor that keeps the guard from passing on an enumeration that collapsed to nothing;
	// it is not an inventory of the routes. Adding a route means raising it, and that is the point: a route
	// that disappears silently would otherwise leave the guard checking less than its name claims.
	minRoutes int
}

func serverConfigurations() []serverConfiguration {
	return []serverConfiguration{
		// block, block/, files, files/
		{name: "export-block", operation: common.OperationExport, mode: config.VolumeModeBlock, minRoutes: 4 * routesPerSuffix},
		{name: "export-filesystem", operation: common.OperationExport, mode: config.VolumeModeFilesystem, minRoutes: 4 * routesPerSuffix},
		// the same four plus finished
		{name: "import-block", operation: common.OperationImport, mode: config.VolumeModeBlock, minRoutes: 5 * routesPerSuffix},
		{name: "import-filesystem", operation: common.OperationImport, mode: config.VolumeModeFilesystem, minRoutes: 5 * routesPerSuffix},
	}
}

func (c serverConfiguration) config() *config.Config {
	return &config.Config{
		Mode:            c.mode,
		Operation:       c.operation,
		DataManagerName: "test-manager",
		URLOpt: config.URLOpt{
			DataManagerNamespace:       "test-ns",
			DataManagerTargetKindShort: "pvc",
			DataManagerTargetName:      "test-target",
		},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// transferCounter stands in for the idle-window timer and records what the transfer accounting reported to
// it. No locking: every test below drives requests one at a time.
type transferCounter struct {
	bytes int
}

func (c *transferCounter) AddTransferredBytes(n int) {
	c.bytes += n
}

// accountingFor builds the same wrapper main hands to the assembly — the real ttl_control middleware, over a
// counter the test can read. The tests use the real one and not a pass-through so that what they exercise is
// the chain the server runs, including where in it the accounting sits.
func accountingFor(counter *transferCounter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return ttl_control.RequestNotifier(next, counter)
	}
}

// refuseToServe is a stand-in handler that fails the test if it is ever reached. A request that was not
// authorized must be answered by the middleware, so reaching a handler at all is the failure — and the 200
// it then writes also breaks the status assertion, so the failure cannot be swallowed.
func refuseToServe(t *testing.T, name string) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the %s handler served %s %s: an unauthorized request must never reach a handler", name, r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
}

// TestEveryRegisteredRouteRefusesAnonymousRequest is the structural guard on the served routes: whatever this
// server registers, a request that carries no credentials is refused by authorization and never reaches a
// handler. It enumerates the routes from the registrar rather than from a list of its own, so a route added
// later is covered without anybody remembering this test.
//
// Its limits, so that the next reader does not take it for more than it is: it sees the routes that go
// through the registrar (TestRoutesAreRegisteredOnlyThroughOnePoint is what keeps that from being bypassed),
// it checks the refusal and not what a handler does after a successful authorization, and it says nothing
// about whether the authorizer itself decides correctly — that is covered by the tests of the authorization
// package. Run with -v to see the per-configuration counts.
func TestEveryRegisteredRouteRefusesAnonymousRequest(t *testing.T) {
	for _, configuration := range serverConfigurations() {
		t.Run(configuration.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			// Both mocks are deliberately left without a single EXPECT: a request with no credentials is
			// refused by the middleware itself, so neither the authorizer nor the server state must be
			// touched, and any call here fails the test on its own.
			authorizer := mock.NewMockUserAuthorizer(ctrl)
			stateSetter := mock.NewMockDataManagerServerStateSetter(ctrl)

			cfg := configuration.config()
			mux := &recordingRouter{ServeMux: http.NewServeMux()}
			served, err := NewDataServerHandler(
				mux,
				cfg,
				refuseToServe(t, "data"),
				// The real end-of-upload handler and not a stand-in, because it is the one route here whose
				// handler answers a wrong method with a 405 — which is the answer that proves a request got
				// past the chain, and it can only appear if the real handler is in place.
				NewFinishedHandler(stateSetter, cfg, discardLogger()),
				authorizer,
				accountingFor(&transferCounter{}),
				discardLogger(),
			)
			require.NoError(t, err)

			require.GreaterOrEqual(t, len(mux.patterns), configuration.minRoutes,
				"the registrar saw %d routes, fewer than the %d this configuration registers: the guard would be checking less than it claims",
				len(mux.patterns), configuration.minRoutes)

			for pattern, reason := range anonymousRoutes {
				assert.Contains(t, mux.patterns, pattern,
					"declared anonymous route %q (%s) is not registered any more: the exception outlived its reason and must go", pattern, reason)
			}

			checked, exempt, probes := 0, 0, 0
			for _, pattern := range mux.patterns {
				if reason, ok := anonymousRoutes[pattern]; ok {
					t.Logf("route %s not checked: declared anonymous (%s)", pattern, reason)
					exempt++

					continue
				}
				checked++

				for _, method := range probeMethods {
					recorder := httptest.NewRecorder()
					served.ServeHTTP(recorder, httptest.NewRequest(method, pattern, nil))
					probes++

					assert.Equal(t, http.StatusUnauthorized, recorder.Code,
						"%s %s: a request without credentials must be refused as unauthorized", method, pattern)
					assert.NotEqual(t, http.StatusMethodNotAllowed, recorder.Code,
						"%s %s: a 405 means a handler answered, so the middleware chain did not run for this route", method, pattern)
				}
			}

			t.Logf("configuration %s: %d routes registered, %d checked, %d declared anonymous, %d probes",
				configuration.name, len(mux.patterns), checked, exempt, probes)
			require.Positive(t, probes, "no route was probed, so this configuration proved nothing")
		})
	}
}

// TestPathWithNoRouteRefusesAnonymousRequest states the other half of the fix: the chain sits in front of the
// router, not behind it. A path nothing is registered for is refused as unauthorized rather than answered
// with a 404, which is what makes authorization something a route cannot be registered without — and it also
// stops an unnamed caller from mapping which paths exist.
func TestPathWithNoRouteRefusesAnonymousRequest(t *testing.T) {
	for _, configuration := range serverConfigurations() {
		t.Run(configuration.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			authorizer := mock.NewMockUserAuthorizer(ctrl)

			served, err := NewDataServerHandler(
				http.NewServeMux(),
				configuration.config(),
				refuseToServe(t, "data"),
				refuseToServe(t, "end-of-upload"),
				authorizer,
				accountingFor(&transferCounter{}),
				discardLogger(),
			)
			require.NoError(t, err)

			recorder := httptest.NewRecorder()
			served.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/no-such-route", nil))

			assert.Equal(t, http.StatusUnauthorized, recorder.Code,
				"an anonymous request to an unregistered path must be refused by the chain before routing")
		})
	}
}

// TestRoutesAreRegisteredOnlyThroughOnePoint keeps the single registration point single. Every route of this
// server is registered by muxAddHandler; a pattern handed straight to the mux would be served all the same,
// but it would be invisible to TestEveryRegisteredRouteRefusesAnonymousRequest, which enumerates what the
// registrar was given. An unwatched route is how the end-of-upload endpoint stayed unauthorized: it had a
// registration function of its own, and nothing connected it to the rule the other routes followed.
//
// So a route is added by calling muxAddHandler with the suffix, never by touching the mux directly.
//
// What this guard sees: calls named Handle or HandleFunc on any selector, in the non-test sources of this
// package, inside a function declaration. What it cannot see: a registration made from another package, one
// made through a value of the router interface passed elsewhere, one made by a method under a different
// name, and — because the walk starts at the function declarations of a file — one written into a package-level
// declaration, say a function literal assigned to a package variable. That last one it would take a walk over
// every declaration to catch, which is not worth it while nothing here is built that way. It is deliberately
// blunt in the other direction — any Handle call outside the registration point is reported, whatever the
// receiver — because a false report costs a comment and a missed one costs an unwatched route.
func TestRoutesAreRegisteredOnlyThroughOnePoint(t *testing.T) {
	const registrationPoint = "muxAddHandler"

	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, ".", func(entry iofs.FileInfo) bool {
		return !strings.HasSuffix(entry.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)

	files, registrations := 0, 0
	for _, pkg := range packages {
		for path, file := range pkg.Files {
			files++

			for _, decl := range file.Decls {
				function, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}

				ast.Inspect(function, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					selector, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc" {
						return true
					}
					registrations++

					assert.Equal(t, registrationPoint, function.Name.Name,
						"%s: %s() registers a route outside %s(); register through %s(suffix, ...) instead, so the route stays visible to the guard on every route",
						fset.Position(call.Pos()), function.Name.Name, registrationPoint, registrationPoint)

					return true
				})
			}

			t.Logf("scanned %s", path)
		}
	}

	t.Logf("%d source files scanned, %d route registrations found", files, registrations)
	require.Positive(t, files, "no sources were scanned, so this guard proved nothing")
	require.Positive(t, registrations, "no route registration was found at all: the scan no longer recognises them")
}

// TestFinishedRouteRequiresAuthorization is the route-level guard for the end-of-upload signal, which used to
// be served with no authorization at all: anyone who could reach the published ingress could end somebody
// else's upload window, indistinguishably from the client doing it. So an anonymous POST is refused and the
// server state is left alone, and an authorized POST still works.
func TestFinishedRouteRequiresAuthorization(t *testing.T) {
	configuration := serverConfiguration{operation: common.OperationImport, mode: config.VolumeModeBlock}
	cfg := configuration.config()

	// Both the short path and the published long one: they are separate registrations, so a fix that reached
	// only one of them would be half a fix.
	paths := map[string]string{
		"short path": "/api/v1/finished",
		"long path":  GetPathPrefix(cfg.URLOpt) + "api/v1/finished",
	}

	for name, path := range paths {
		t.Run(name+"/anonymous POST is refused and the server state is untouched", func(t *testing.T) {
			ctrl := gomock.NewController(t)
			// No EXPECT on either mock: the request must be stopped before anything is authenticated and
			// long before the import is declared finished.
			authorizer := mock.NewMockUserAuthorizer(ctrl)
			stateSetter := mock.NewMockDataManagerServerStateSetter(ctrl)

			served := finishedTestHandler(t, cfg, stateSetter, authorizer, &transferCounter{})

			recorder := httptest.NewRecorder()
			served.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))

			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		})

		t.Run(name+"/authorized POST reports the upload finished", func(t *testing.T) {
			ctrl := gomock.NewController(t)
			authorizer := mock.NewMockUserAuthorizer(ctrl)
			authorizer.EXPECT().AuthenticateUserByToken(gomock.Any(), "test-token").Return(true, "user", []string{"group"}, nil)
			authorizer.EXPECT().
				AuthorizeUser(gomock.Any(), common.OperationImport, cfg.URLOpt.DataManagerNamespace, "user", []string{"group"}).
				Return(true, "", nil)

			stateSetter := mock.NewMockDataManagerServerStateSetter(ctrl)
			stateSetter.EXPECT().
				SetServerState(gomock.Any(), common.OperationImport, cfg.URLOpt.DataManagerNamespace, cfg.DataManagerName, common.ServerStateFinished).
				Return(nil)

			served := finishedTestHandler(t, cfg, stateSetter, authorizer, &transferCounter{})

			request := httptest.NewRequest(http.MethodPost, path, nil)
			request.Header.Set("Authorization", "Bearer test-token")

			recorder := httptest.NewRecorder()
			served.ServeHTTP(recorder, request)

			// 204 also settles the one thing that could have made the fix break the client: the
			// required-header check now sits in front of this route too, and it lets a non-PUT through
			// untouched, so a completion that carries no upload headers still succeeds.
			assert.Equal(t, http.StatusNoContent, recorder.Code)
		})
	}
}

func finishedTestHandler(
	t *testing.T,
	cfg *config.Config,
	stateSetter *mock.MockDataManagerServerStateSetter,
	authorizer *mock.MockUserAuthorizer,
	counter *transferCounter,
) http.Handler {
	t.Helper()

	served, err := NewDataServerHandler(
		http.NewServeMux(),
		cfg,
		refuseToServe(t, "data"),
		NewFinishedHandler(stateSetter, cfg, discardLogger()),
		authorizer,
		accountingFor(counter),
		discardLogger(),
	)
	require.NoError(t, err)

	return served
}

// TestRefusalOfAnUnrecognisedCallerIsNotTransferActivity pins the definition of activity the idle window
// runs on: what a caller the server recognised makes the server do. The accounting sits immediately inside
// authorization, so the bytes of a refusal written to an unknown caller never reach it — otherwise one
// anonymous request per idle period, from anywhere that can reach the published endpoint, would keep the pod,
// its volume and the transfer window alive for as long as the sender cared to keep sending, and would refresh
// the activity timestamp while doing it. Past authorization the accounting is unconditional, which is what
// keeps the window open under a client retrying a failed completion.
func TestRefusalOfAnUnrecognisedCallerIsNotTransferActivity(t *testing.T) {
	configuration := serverConfiguration{operation: common.OperationImport, mode: config.VolumeModeBlock}
	cfg := configuration.config()

	authorize := func(authorizer *mock.MockUserAuthorizer) {
		authorizer.EXPECT().AuthenticateUserByToken(gomock.Any(), "test-token").Return(true, "user", []string{"group"}, nil)
		authorizer.EXPECT().
			AuthorizeUser(gomock.Any(), common.OperationImport, cfg.URLOpt.DataManagerNamespace, "user", []string{"group"}).
			Return(true, "", nil)
	}

	t.Run("the refusal of a caller without credentials moves nothing", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		authorizer := mock.NewMockUserAuthorizer(ctrl)
		stateSetter := mock.NewMockDataManagerServerStateSetter(ctrl)

		counter := &transferCounter{}
		served := finishedTestHandler(t, cfg, stateSetter, authorizer, counter)

		recorder := httptest.NewRecorder()
		served.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/finished", nil))

		require.Equal(t, http.StatusUnauthorized, recorder.Code)
		assert.NotEmpty(t, recorder.Body.String(), "the refusal must carry a body, or this proves nothing about counting one")
		assert.Zero(t, counter.bytes, "the bytes of a refusal written to an unknown caller must not reach the idle window")
	})

	t.Run("the refusal of a recognised caller is activity", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		authorizer := mock.NewMockUserAuthorizer(ctrl)
		authorize(authorizer)

		// The completion the client will retry: the state write fails, the caller gets a 500.
		stateSetter := mock.NewMockDataManagerServerStateSetter(ctrl)
		stateSetter.EXPECT().
			SetServerState(gomock.Any(), common.OperationImport, cfg.URLOpt.DataManagerNamespace, cfg.DataManagerName, common.ServerStateFinished).
			Return(errors.New("state write failed"))

		counter := &transferCounter{}
		served := finishedTestHandler(t, cfg, stateSetter, authorizer, counter)

		request := httptest.NewRequest(http.MethodPost, "/api/v1/finished", nil)
		request.Header.Set("Authorization", "Bearer test-token")

		recorder := httptest.NewRecorder()
		served.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Positive(t, counter.bytes, "an answer to a recognised caller is activity: the window must not close under a retry")
	})

	t.Run("a successful completion moves nothing either", func(t *testing.T) {
		// Not a refusal, but the same measurement, and it is what makes the completion route safe to keep
		// inside the accounting: a 204 with no body cannot postpone the deadline it is the signal to close.
		ctrl := gomock.NewController(t)
		authorizer := mock.NewMockUserAuthorizer(ctrl)
		authorize(authorizer)

		stateSetter := mock.NewMockDataManagerServerStateSetter(ctrl)
		stateSetter.EXPECT().
			SetServerState(gomock.Any(), common.OperationImport, cfg.URLOpt.DataManagerNamespace, cfg.DataManagerName, common.ServerStateFinished).
			Return(nil)

		counter := &transferCounter{}
		served := finishedTestHandler(t, cfg, stateSetter, authorizer, counter)

		request := httptest.NewRequest(http.MethodPost, "/api/v1/finished", nil)
		request.Header.Set("Authorization", "Bearer test-token")

		recorder := httptest.NewRecorder()
		served.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusNoContent, recorder.Code)
		assert.Zero(t, counter.bytes)
	})
}
