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
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/deckhouse/sds-common-lib/fs/mockfs"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/authorization"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/test"
)

// File contents are markers rather than plausible data, so that a response can be checked for content
// the exporter must never serve. The exporter's contract is that a path never escapes the exported
// root and that symlinks are followed neither at the end nor in the middle of a path, so a marker
// belonging to a file outside the exported root appearing in a response body is a containment
// violation.
//
// The markers are deliberately not path-shaped: a symlink request legitimately reports its target
// path ("/outer_dir") in the body and headers, and that must not be mistaken for leaked content.
const (
	markerShareFile       = "MARKER-SHARE-FILE"
	markerShareNestedFile = "MARKER-SHARE-NESTED-FILE"
	markerBlockDevice     = "MARKER-BLOCK-DEVICE"
	markerOuterDirFile    = "MARKER-OUTER-DIR-FILE"
	markerOuterRootFile   = "MARKER-OUTER-ROOT-FILE"
)

// allowAllAuthorizer replaces the gomock-generated mock, which cannot be hoisted out of the fuzz
// function: the generated mock calls TestReporter.Helper on every invocation, and *testing.F methods
// panic when called inside the fuzz target, so the controller would have to be rebuilt on every
// iteration. Measured, that cost ~9.5us and 49 allocations per iteration for no oracle value, since
// every expectation is AnyTimes().
type allowAllAuthorizer struct{}

func (allowAllAuthorizer) AuthenticateUserByToken(context.Context, string) (bool, string, []string, error) {
	return true, "user", []string{"group"}, nil
}

func (allowAllAuthorizer) AuthorizeUser(context.Context, common.Operation, string, string, []string) (bool, string, error) {
	return true, "", nil
}

// newFuzzFS builds the fixture below. require rather than assert: a half-built fixture would make
// every iteration run against a broken tree and report meaningless results.
//
//	/
//	├── block                        <- exported root in block mode
//	├── sharedir                     <- exported root in filesystem mode
//	│   ├── dir
//	│   │   ├── nested_dir (empty)
//	│   │   ├── nested_file
//	│   │   └── nested_link -> ../file
//	│   ├── dir_link -> /outer_dir   <- escapes the exported root
//	│   ├── file
//	│   └── file_link -> /foo/bar    <- dangling
//	├── outer_dir
//	│   └── file                     <- must never be served
//	└── outer_file                   <- must never be served
func newFuzzFS(t require.TestingT) *mockfs.MockFS {
	fsys, err := mockfs.NewFsMock()
	require.NoError(t, err)

	const gid uint32 = 123
	const uid uint32 = 456

	fsys.DefaultSys.Gid = gid
	fsys.DefaultSys.Uid = uid

	block, err := fsys.CreateFile("/block", os.ModeDevice)
	require.NoError(t, err)
	test.SetContent(block, markerBlockDevice)

	_, err = fsys.CreateFile("/sharedir", os.ModeDir)
	require.NoError(t, err)

	_, err = fsys.CreateFile("/sharedir/dir", os.ModeDir)
	require.NoError(t, err)

	_, err = fsys.CreateFile("/sharedir/dir/nested_dir", os.ModeDir|0o750)
	require.NoError(t, err)

	nestedFile, err := fsys.CreateFile("/sharedir/dir/nested_file", 0o664)
	require.NoError(t, err)
	test.SetContent(nestedFile, markerShareNestedFile)

	nestedLink, err := fsys.CreateFile("/sharedir/dir/nested_link", os.ModeSymlink|0o664)
	require.NoError(t, err)
	nestedLink.LinkSource = "../file"

	dirLink, err := fsys.CreateFile("/sharedir/dir_link", os.ModeSymlink)
	require.NoError(t, err)
	dirLink.LinkSource = "/outer_dir"

	file, err := fsys.CreateFile("/sharedir/file", 0o664)
	require.NoError(t, err)
	test.SetContent(file, markerShareFile)

	fileLink, err := fsys.CreateFile("/sharedir/file_link", os.ModeSymlink)
	require.NoError(t, err)
	fileLink.LinkSource = "/foo/bar"

	_, err = fsys.CreateFile("/outer_dir", os.ModeDir)
	require.NoError(t, err)

	outerDirFile, err := fsys.CreateFile("/outer_dir/file", 0o664)
	require.NoError(t, err)
	test.SetContent(outerDirFile, markerOuterDirFile)

	outerFile, err := fsys.CreateFile("/outer_file", 0o664)
	require.NoError(t, err)
	test.SetContent(outerFile, markerOuterRootFile)

	return fsys
}

// The fixture is built once and shared by every iteration, which is what allows the handler and the
// mux to be built once as well. F.Fuzz documents that the fuzz function should not depend on shared
// state, so the fixture is snapshotted and re-checked after every iteration: the export handlers only
// read, and this turns that assumption into a checked invariant. It fails loudly if a handler that
// writes is ever driven from these targets, which would otherwise leak state between iterations and
// make failures unreproducible.
var fixtureFiles = []string{
	"/block",
	"/sharedir/file",
	"/sharedir/dir/nested_file",
	"/outer_dir/file",
	"/outer_file",
}

func snapshotFixture(t require.TestingT, fsys *mockfs.MockFS) map[string]int64 {
	sizes := make(map[string]int64, len(fixtureFiles))
	for _, path := range fixtureFiles {
		fi, err := fsys.Lstat(path)
		require.NoError(t, err)
		sizes[path] = fi.Size()
	}
	return sizes
}

// Returns an error rather than failing the test directly, so that the concurrency test can call it
// from a worker goroutine, where t.Fatalf is not allowed.
func checkFixtureUnchanged(fsys *mockfs.MockFS, want map[string]int64) error {
	for _, path := range fixtureFiles {
		fi, err := fsys.Lstat(path)
		if err != nil {
			return fmt.Errorf("fixture file %s disappeared during the request: %w", path, err)
		}
		if fi.Size() != want[path] {
			return fmt.Errorf("fixture file %s changed size during the request: want %d, got %d", path, want[path], fi.Size())
		}
	}

	return nil
}

// methods includes methods the exporter does not support; those must be rejected rather than served.
var methods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
	http.MethodTrace,
	http.MethodConnect,
}

// buildRequest reports ok=false for an input http.NewRequest rejects, instead of calling t.Skip: such
// an input is simply uninteresting, and returning from the fuzz function costs nothing while t.Skip
// unwinds the goroutine through runtime.Goexit on every malformed URL.
func buildRequest(
	authType string,
	basicCreds string,
	bearerToken string,
	wrongAuth string,
	rawURL string,
	methodIdx uint8,
	body []byte,
) (*http.Request, bool) {
	// uint8 rather than int: only len(methods) values are meaningful, so a wider type only gives the
	// engine more bits to waste. It also removes the negation overflow an int index invited, where
	// -math.MinInt64 stays negative and indexes out of range.
	method := methods[int(methodIdx)%len(methods)]

	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, false
	}

	switch strings.ToLower(authType) {
	case "basic":
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(basicCreds)))
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	default:
		req.Header.Set("Authorization", wrongAuth)
	}

	return req, true
}

type fuzzSeed struct {
	authType    string
	basicCreds  string
	bearerToken string
	wrongAuth   string
	rawURL      string
	methodIdx   uint8
	body        []byte
	failSeed    int64
	failProb    byte
}

// Both targets get the whole seed corpus: an input aimed at the block routes still exercises the
// filesystem target's wrong-prefix informers, and each target's own coverage feedback keeps whatever
// turns out to matter for it.
func seedCorpus() []fuzzSeed {
	return []fuzzSeed{
		// Filesystem routes, well-formed prefix, auth and URL.
		{"bearer", "", "token-123", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/file", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/file", 1, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/file", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/file?attribute=stat&attribute=hash.md5", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/file/", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/bad_file", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/file_link", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir_link", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir_link", 1, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/", 1, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/dir/", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/?attribute=stat&attribute=hash.md5", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_file", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_file", 1, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_file?attribute=hash.md5", 0, []byte("12345"), 1234, 5},
		{"bearer", "d", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_link", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_dir/", 0, []byte("12345"), 1234, 5},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_dir", 0, []byte("12345"), 1234, 5},
		// Paths aimed at the exported root's boundary.
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/../outer_file", 0, []byte("12345"), 1234, 0},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir_link/file", 0, []byte("12345"), 1234, 0},
		{"bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir_link/", 0, []byte("12345"), 1234, 0},
		// Adversarial auth, methods and encodings.
		{"basic", "user:pass", "", "", "https://foo.bar/api/v1/files/", 0, []byte("12345"), 174757, 20},
		{"bearer", "", "dXNlcjpwYXNzd2Q=", "", "https://foo.bar/hello/v1/files/dir/?attribute=stat", 1, []byte("hello"), 573558, 0},
		{"wrong", "", "", " abcdйцукен///⚺⪣⾓␜ⅷⱦ⣠ⵉ⾛⃚⴦ⶭ", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/dir_link?attribute=stat&attribute=hash.md5", 2, []byte("qwerty"), 88478, 255},
		{"basic", "user:pass", "", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/file/", 3, []byte("284756"), 8474, 100},
		{"bearer", "", "113edec49eaa=", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/../../foo", 0, []byte("84831"), 5875, 50},
		{"bearer", "", "⡋⃤⣿⋎⭔┛⺗⏯╽", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/ abcdйцукен///⚺⪣⾓␜ⅷⱦ⣠ⵉ⾛⃚⴦ⶭ", 0, []byte("48575"), 1234, 10},
		{"wrong", "", "", "", "https://foo.bar/api/v1/files/file", 0, []byte("12345"), 1234, 0},
		// failProb is 0 on purpose: the no-5xx invariant only applies without injected failures, so a
		// credential the exporter rejects has to be seeded with injection off to be checked at all. This is
		// the shape that caught the unsupported-credential path being reported as a 500 rather than a 401,
		// which the fuzzer had not reached on its own because it needs the scheme and failProb to line up.
		{"basic", "user:pass", "", "", "https://foo.bar/api/v1/files/file", 0, []byte("12345"), 1234, 0},
		{"basic", "user:pass", "", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/dir/", 0, []byte("12345"), 1234, 0},
		{"basic", "", "", "", "https://foo.bar/api/v1/files/file", 1, []byte("12345"), 1234, 0},
		{"wrong", "", "", "Negotiate abcdef", "https://foo.bar/api/v1/files/file", 0, []byte("12345"), 1234, 0},
		{"wrong", "", "", "Bearer ", "https://foo.bar/api/v1/files/file", 0, []byte("12345"), 1234, 0},
		// Block routes.
		{"bearer", "", "some-token", "", "https://foo.bar/api/v1/block", 0, []byte("12345"), 4567, 5},
		{"bearer", "", "some-token", "", "https://foo.bar/api/v1/block", 1, []byte("12345"), 4567, 5},
		{"bearer", "", "some-token", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/block", 0, []byte("12345"), 4567, 5},
		{"bearer", "", "some-token", "", "https://foo.bar/api/v1/block/", 0, []byte("12345"), 4567, 5},
		{"bearer", "", "some-token", "", "https://foo.bar/api/v1/block/file", 0, []byte("12345"), 4567, 5},
		{"wrong", "", "", "somerandom", "https://foo.bar/de-ns/de-kind/de-name/api/v1/block", 1, []byte("hello"), 2345, 4},
		{"basic", "user:password", "", "", "https://foo.bar/api/v1/block/file", 0, []byte("12345"), 174757, 10},
		{"bearer", "", "113edec49eaa=", "", "https://foo.bar/dir/de-ns/de-kind/de-name/api/v1/block", 1, []byte("hello"), 434, 0},
		{"bearer", "", "FMfjh84frk=", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/block/☰∉Ⅿ⏵⡪↿⊡/../⪭⬆⮖Ⰶ⭆⫳✨", 1, []byte("hello"), 434, 0},
	}
}

func addSeedCorpus(f *testing.F) {
	for _, s := range seedCorpus() {
		f.Add(s.authType, s.basicCreds, s.bearerToken, s.wrongAuth, s.rawURL, s.methodIdx, s.body, s.failSeed, s.failProb)
	}
}

func fuzzURLOpt() config.URLOpt {
	return config.URLOpt{
		DataManagerNamespace:       "de-ns",
		DataManagerTargetKindShort: "de-kind",
		DataManagerTargetName:      "de-name",
	}
}

// checkResponse holds the exporter to the properties it claims, rather than only to not crashing. It
// returns an error rather than failing the test directly, so that the concurrency test can call it
// from a worker goroutine, where t.Fatalf is not allowed.
func checkResponse(rr *httptest.ResponseRecorder, req *http.Request, forbidden []string, failProb byte) error {
	body := rr.Body.String()

	// Containment: content of a file outside the exported root must never reach the client.
	for _, marker := range forbidden {
		if strings.Contains(body, marker) {
			return fmt.Errorf("response leaked content from outside the exported root: marker %s, method %s, path %q, status %d",
				marker, req.Method, req.URL.Path, rr.Code)
		}
	}

	// A 5xx is only legitimate when a filesystem failure was injected. Without injection, every
	// rejection is caused by the request itself, which is a 4xx.
	if failProb == 0 && rr.Code >= http.StatusInternalServerError {
		return fmt.Errorf("server error without an injected filesystem failure: status %d, method %s, path %q, body %s",
			rr.Code, req.Method, req.URL.Path, firstLine(body))
	}

	if rr.Code < http.StatusOK || rr.Code >= http.StatusMultipleChoices {
		return nil
	}

	// Success implies a read: the exporter serves GET and HEAD, and every other method is rejected
	// either by the exporter or by the wrong-prefix informers.
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return fmt.Errorf("method %s succeeded with status %d on path %q, expected a rejection",
			req.Method, rr.Code, req.URL.Path)
	}

	// Success implies a credential was presented. An empty Authorization header must be rejected as
	// Unauthorized, which is what authorization.Authorize documents.
	if req.Header.Get("Authorization") == "" {
		return fmt.Errorf("request without an Authorization header succeeded with status %d on path %q",
			rr.Code, req.URL.Path)
	}

	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}

	const limit = 200
	if len(s) > limit {
		s = s[:limit] + "..."
	}

	return fmt.Sprintf("%q", s)
}

// fuzzExportTarget drives one exporter. Everything that does not depend on the fuzzed input — the
// fixture, the handler, the authorization middleware and the mux — is built once: measured, building
// the two muxes per iteration cost 58us and 390 allocations against 750ns of actual request serving,
// and hoisting it cut a fixed 50k-iteration run from 41.4s to 26.9s.
func fuzzExportTarget(f *testing.F, mode config.VolumeMode, root string, forbidden []string) {
	addSeedCorpus(f)

	opt := fuzzURLOpt()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fsys := newFuzzFS(f)
	fixture := snapshotFixture(f, fsys)

	exporter, err := NewExporterHandler(fsys, root, mode, logger)
	require.NoError(f, err)

	authorized := authorization.Authorize(
		exporter,
		allowAllAuthorizer{},
		common.OperationExport,
		opt.DataManagerNamespace,
	)

	mux := http.NewServeMux()
	switch mode {
	case config.VolumeModeFilesystem:
		MuxAddFSHandler(mux, opt, authorized, logger)
	case config.VolumeModeBlock:
		MuxAddBlockHandler(mux, opt, authorized, logger)
	default:
		f.Fatalf("unsupported volume mode %v", mode)
	}

	f.Fuzz(func(
		t *testing.T,
		authType string,
		basicCreds string,
		bearerToken string,
		wrongAuth string,
		rawURL string,
		methodIdx uint8,
		body []byte,
		failSeed int64,
		failProb byte,
	) {
		req, ok := buildRequest(authType, basicCreds, bearerToken, wrongAuth, rawURL, methodIdx, body)
		if !ok {
			return
		}

		fsys.Failer = mockfs.NewProbabilityFailer(failSeed, float64(failProb)/255.0)

		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		// The oracles read the fixture themselves, so injection has to stop first — otherwise a check
		// could fail on an injected error rather than on the property it is testing.
		fsys.Failer = nil

		if err := checkResponse(rr, req, forbidden, failProb); err != nil {
			t.Fatal(err)
		}

		if err := checkFixtureUnchanged(fsys, fixture); err != nil {
			t.Fatal(err)
		}
	})
}

// Split per exporter so that each target keeps its own corpus and coverage signal, and so a failure
// names the exporter that produced it. The forbidden markers are those of every file outside the
// target's exported root.
func FuzzExportFilesystemHandler(f *testing.F) {
	fuzzExportTarget(f, config.VolumeModeFilesystem, "/sharedir", []string{
		markerOuterDirFile,
		markerOuterRootFile,
		markerBlockDevice,
	})
}

func FuzzExportBlockHandler(f *testing.F) {
	fuzzExportTarget(f, config.VolumeModeBlock, "/block", []string{
		markerOuterDirFile,
		markerOuterRootFile,
		markerShareFile,
		markerShareNestedFile,
	})
}
