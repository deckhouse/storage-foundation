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

package importfilesystem_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/sds-common-lib/fs/real"
	api "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	importfilesystem "github.com/deckhouse/storage-foundation/images/data-exporter/internal/import_filesystem"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/repository"
)

const (
	outerFileSize     = 12345
	outerTempFileSize = 777
)

type importTree struct {
	top  string
	root string
}

func newImportTree(t *testing.T) importTree {
	t.Helper()

	top := t.TempDir()
	root := filepath.Join(top, "x", "volume")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dir"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(top, "x", "outer_file"), make([]byte, outerFileSize), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(top, "x", "outer_temp.tempupload"), make([]byte, outerTempFileSize), 0o640))

	return importTree{top: top, root: root}
}

func (tree importTree) requireNothingWrittenOutsideRoot(t *testing.T) {
	t.Helper()

	var outside []string
	require.NoError(t, filepath.WalkDir(tree.top, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == tree.root {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(tree.top, path)
		if err != nil {
			return err
		}
		outside = append(outside, rel)
		return nil
	}))
	require.ElementsMatch(t, []string{".", "x", "x/outer_file", "x/outer_temp.tempupload"}, outside)
}

func newConfig() *config.Config {
	return &config.Config{
		Mode:            config.VolumeModeFilesystem,
		Operation:       common.OperationImport,
		DataManagerName: "import",
		URLOpt:          config.URLOpt{DataManagerNamespace: "project", DataManagerTargetKindShort: "pvc", DataManagerTargetName: "volume"},
	}
}

func newClient(t *testing.T, state common.ServerState, funcs interceptor.Funcs) *repository.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	di := &api.DataImport{
		ObjectMeta: metav1.ObjectMeta{Namespace: "project", Name: "import"},
		Status:     api.DataExportImportStatus{ServerState: string(state)},
	}

	return &repository.Client{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(di).
		WithObjects(di).
		WithInterceptorFuncs(funcs).
		Build()}
}

func newHandlerWithClient(t *testing.T, root string, client *repository.Client) http.Handler {
	t.Helper()

	handler, err := importfilesystem.NewImportFilesystemHandler(real.GetOS(), root, slog.New(slog.DiscardHandler), client, newConfig(), client)
	require.NoError(t, err)

	return handler
}

func newHandler(t *testing.T, root string) http.Handler {
	t.Helper()
	return newHandlerWithClient(t, root, newClient(t, "", interceptor.Funcs{}))
}

func newPutRequest(urlPath, content string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(content))
	req.URL.Path = urlPath
	req.Header.Set("X-Content-Length", strconv.Itoa(len(content)))
	req.Header.Set("X-Offset", "0")
	req.Header.Set("X-Attribute-Permissions", "0640")
	req.Header.Set("X-Attribute-Uid", strconv.Itoa(os.Getuid()))
	req.Header.Set("X-Attribute-Gid", strconv.Itoa(os.Getgid()))

	return req
}

func newHeadRequest(urlPath string) *http.Request {
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	req.URL.Path = urlPath

	return req
}

func TestPutPathClimbingAboveRootStaysInsideRoot(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"../planted", "planted"},
		{"../outside/planted", "outside/planted"},
		{"../../planted", "planted"},
		{"dir/../../planted", "planted"},
		{"./../planted", "planted"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			tree := newImportTree(t)
			handler := newHandler(t, tree.root)
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, newPutRequest(tc.path, "planted"))

			tree.requireNothingWrittenOutsideRoot(t)
			require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
			content, err := os.ReadFile(filepath.Join(tree.root, tc.want))
			require.NoError(t, err)
			require.Equal(t, "planted", string(content))
		})
	}
}

func TestPutRootItselfIsRefused(t *testing.T) {
	for _, path := range []string{"", ".", "/", "dir/..", ".."} {
		t.Run(path, func(t *testing.T) {
			tree := newImportTree(t)
			handler := newHandler(t, tree.root)
			before, err := os.Stat(tree.root)
			require.NoError(t, err)
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, newPutRequest(path, "planted"))

			require.Equal(t, http.StatusBadRequest, rr.Code, "body=%q", rr.Body.String())
			tree.requireNothingWrittenOutsideRoot(t)
			after, err := os.Stat(tree.root)
			require.NoError(t, err)
			require.Equal(t, before.Mode(), after.Mode())
		})
	}
}

func TestHeadPathClimbingAboveRootDisclosesNothingOutsideRoot(t *testing.T) {
	for _, tc := range []struct {
		path string
		want int
	}{
		{"../outer_file", http.StatusNotFound},
		{"../../x/outer_file", http.StatusNotFound},
		{"dir/../../outer_file", http.StatusNotFound},
		{"./../outer_file", http.StatusNotFound},
		{"../outer_temp", http.StatusNotFound},
		{"..", http.StatusBadRequest},
	} {
		t.Run(tc.path, func(t *testing.T) {
			tree := newImportTree(t)
			handler := newHandler(t, tree.root)
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, newHeadRequest(tc.path))

			require.NotEqual(t, strconv.Itoa(outerFileSize), rr.Header().Get("Content-Length"))
			require.Empty(t, rr.Header().Get("X-Next-Offset"))
			require.Equal(t, tc.want, rr.Code)
		})
	}
}

func TestPathWithDotDotInsideRootStillWorks(t *testing.T) {
	tree := newImportTree(t)
	handler := newHandler(t, tree.root)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, newPutRequest("dir/../file", "content"))
	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())

	content, err := os.ReadFile(filepath.Join(tree.root, "file"))
	require.NoError(t, err)
	require.Equal(t, "content", string(content))

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, newPutRequest("/dir/nested/../nested_file", "nested"))
	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())

	content, err = os.ReadFile(filepath.Join(tree.root, "dir", "nested_file"))
	require.NoError(t, err)
	require.Equal(t, "nested", string(content))

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, newPutRequest("/../top_file", "top"))
	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())

	content, err = os.ReadFile(filepath.Join(tree.root, "top_file"))
	require.NoError(t, err)
	require.Equal(t, "top", string(content))

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, newHeadRequest("dir/../file"))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, strconv.Itoa(len("content")), rr.Header().Get("Content-Length"))

	tree.requireNothingWrittenOutsideRoot(t)
}

var errInjected = errors.New("injected failure")

func newChunkRequest(urlPath string, offset, total int, body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/", body)
	req.URL.Path = urlPath
	req.Header.Set("X-Offset", strconv.Itoa(offset))
	if total >= 0 {
		req.Header.Set("X-Content-Length", strconv.Itoa(total))
	}

	return req
}

func newChunk(urlPath string, offset, total int, body string) *http.Request {
	return newChunkRequest(urlPath, offset, total, strings.NewReader(body))
}

func newRequest(method, urlPath string) *http.Request {
	req := httptest.NewRequest(method, "/", nil)
	req.URL.Path = urlPath

	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	return rr
}

func requireFileContent(t *testing.T, path, want string) {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(content))
}

func requireNotExist(t *testing.T, path string) {
	t.Helper()

	_, err := os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func requireRootEmpty(t *testing.T, root string) {
	t.Helper()

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestNewImportFilesystemHandlerRefusesUnusableRoot(t *testing.T) {
	top := t.TempDir()
	file := filepath.Join(top, "file")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0o640))

	for _, tc := range []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "relative path", path: "volume", wantErr: "path must be absolute"},
		{name: "relative path climbing up", path: "../volume", wantErr: "path must be absolute"},
		{name: "regular file", path: file, wantErr: "not a directory"},
		{name: "missing directory", path: filepath.Join(top, "missing"), wantErr: "no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newClient(t, "", interceptor.Funcs{})

			handler, err := importfilesystem.NewImportFilesystemHandler(real.GetOS(), tc.path, slog.New(slog.DiscardHandler), client, newConfig(), client)

			require.ErrorContains(t, err, tc.wantErr)
			require.Nil(t, handler)
		})
	}
}

func TestNewImportFilesystemHandlerFailsWithoutDataImport(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	client := &repository.Client{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	handler, err := importfilesystem.NewImportFilesystemHandler(real.GetOS(), t.TempDir(), slog.New(slog.DiscardHandler), client, newConfig(), client)

	require.True(t, apierrors.IsNotFound(err), "err=%v", err)
	require.Nil(t, handler)
}

func TestNewImportFilesystemHandlerCleansRootPath(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(t, root+"/./dir/..//")

	require.Equal(t, http.StatusCreated, serve(handler, newChunk("file", 0, 4, "data")).Code)
	requireFileContent(t, filepath.Join(root, "file"), "data")
}

func TestServeHTTPRefusesEverythingAfterImportFinished(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0o640))
	handler := newHandlerWithClient(t, root, newClient(t, common.ServerStateFinished, interceptor.Funcs{}))

	for _, req := range []*http.Request{
		newChunk("other", 0, 4, "data"),
		newRequest(http.MethodHead, "file"),
		newRequest(http.MethodPost, ""),
		newRequest(http.MethodGet, "file"),
	} {
		t.Run(req.Method, func(t *testing.T) {
			require.Equal(t, http.StatusForbidden, serve(handler, req).Code)
		})
	}
	requireNotExist(t, filepath.Join(root, "other"))
	requireNotExist(t, filepath.Join(root, "other.tempupload"))
}

func TestServeHTTPRefusesUnsupportedMethods(t *testing.T) {
	handler := newHandler(t, t.TempDir())

	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			require.Equal(t, http.StatusMethodNotAllowed, serve(handler, newRequest(method, "file")).Code)
		})
	}
}

func TestHeadOnDirectoryIsRefused(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "dir"), 0o750))
	handler := newHandler(t, root)

	for _, path := range []string{"", "/", "dir/"} {
		t.Run(path, func(t *testing.T) {
			require.Equal(t, http.StatusBadRequest, serve(handler, newRequest(http.MethodHead, path)).Code)
		})
	}
}

func TestHeadReportsUploadProgress(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "partial.tempupload"), []byte("abc"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(root, "done"), []byte("abcdef"), 0o640))
	// A finished file next to a stale partial upload of the same name: the partial upload wins.
	require.NoError(t, os.WriteFile(filepath.Join(root, "both"), []byte("abcdef"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(root, "both.tempupload"), []byte("ab"), 0o640))
	handler := newHandler(t, root)

	for _, tc := range []struct {
		path              string
		wantCode          int
		wantNextOffset    string
		wantContentLength string
	}{
		{path: "partial", wantCode: http.StatusOK, wantNextOffset: "3"},
		{path: "done", wantCode: http.StatusOK, wantContentLength: "6"},
		{path: "both", wantCode: http.StatusOK, wantNextOffset: "2"},
		{path: "missing", wantCode: http.StatusNotFound, wantContentLength: "0"},
		{path: "dir/missing", wantCode: http.StatusNotFound, wantContentLength: "0"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rr := serve(handler, newRequest(http.MethodHead, tc.path))

			require.Equal(t, tc.wantCode, rr.Code)
			require.Equal(t, tc.wantNextOffset, rr.Header().Get("X-Next-Offset"))
			require.Equal(t, tc.wantContentLength, rr.Header().Get("Content-Length"))
		})
	}
}

func TestHeadFailsWhenFileCannotBeChecked(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(t, root)

	// The name fits the limit of a path component, the name with the temporary suffix does not.
	rr := serve(handler, newRequest(http.MethodHead, strings.Repeat("n", 250)))

	require.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Equal(t, "0", rr.Header().Get("Content-Length"))
	require.Empty(t, rr.Header().Get("X-Next-Offset"))
}

func TestPutResumesThroughTemporaryFile(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(t, root)
	final := filepath.Join(root, "dir", "file")
	temp := final + ".tempupload"

	rr := serve(handler, newChunk("dir/file", 3, 10, "34567"))
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Equal(t, "0", rr.Header().Get("X-Expected-Offset"))
	requireNotExist(t, temp)

	rr = serve(handler, newChunk("dir/file", 0, 10, "01234"))
	require.Equal(t, http.StatusNoContent, rr.Code, "body=%q", rr.Body.String())
	require.Equal(t, "5", rr.Header().Get("X-Next-Offset"))
	requireFileContent(t, temp, "01234")
	requireNotExist(t, final)

	rr = serve(handler, newRequest(http.MethodHead, "dir/file"))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "5", rr.Header().Get("X-Next-Offset"))

	for _, offset := range []int{0, 3, 7} {
		rr = serve(handler, newChunk("dir/file", offset, 10, "ZZZZZ"))
		require.Equal(t, http.StatusConflict, rr.Code, "offset %d", offset)
		require.Equal(t, "5", rr.Header().Get("X-Expected-Offset"), "offset %d", offset)
	}
	requireFileContent(t, temp, "01234")

	rr = serve(handler, newChunk("dir/file", 5, 10, "56789"))
	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
	requireFileContent(t, final, "0123456789")
	requireNotExist(t, temp)

	rr = serve(handler, newRequest(http.MethodHead, "dir/file"))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "10", rr.Header().Get("Content-Length"))
	require.Empty(t, rr.Header().Get("X-Next-Offset"))
}

func TestPutResumesTemporaryFileLeftByEarlierServer(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.tempupload"), []byte("0123"), 0o640))
	handler := newHandler(t, root)

	rr := serve(handler, newChunk("file", 0, 8, "0123"))
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Equal(t, "4", rr.Header().Get("X-Expected-Offset"))

	rr = serve(handler, newChunk("file", 4, 8, "4567"))
	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
	requireFileContent(t, filepath.Join(root, "file"), "01234567")
	requireNotExist(t, filepath.Join(root, "file.tempupload"))
}

func TestPutOfCompleteFileIsRefused(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(t, root)
	require.Equal(t, http.StatusCreated, serve(handler, newChunk("file", 0, 4, "data")).Code)

	for _, offset := range []int{0, 4} {
		rr := serve(handler, newChunk("file", offset, 4, "ZZZZ"))
		require.Equal(t, http.StatusConflict, rr.Code, "offset %d", offset)
		require.Empty(t, rr.Header().Get("X-Expected-Offset"), "offset %d", offset)
	}
	requireFileContent(t, filepath.Join(root, "file"), "data")
	requireNotExist(t, filepath.Join(root, "file.tempupload"))
}

func TestPutAppliesAttributesToFinalFile(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(t, root)
	mtime := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
	uid, gid := os.Getuid(), os.Getgid()
	if os.Geteuid() == 0 {
		uid, gid = 4321, 8765
	}

	req := newChunk("file", 0, 4, "data")
	req.Header.Set("X-Attribute-Permissions", "0604")
	req.Header.Set("X-Attribute-Uid", strconv.Itoa(uid))
	req.Header.Set("X-Attribute-Gid", strconv.Itoa(gid))
	req.Header.Set("X-Attribute-ModTime", mtime.Format(time.RFC3339))
	rr := serve(handler, req)
	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())

	fi, err := os.Lstat(filepath.Join(root, "file"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o604), fi.Mode())
	require.True(t, mtime.Equal(fi.ModTime()), "want %v, got %v", mtime, fi.ModTime())
	if os.Geteuid() != 0 {
		t.Log("not root: owner is set to the current user, so it is not asserted")
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Equal(t, uint32(uid), st.Uid)
	require.Equal(t, uint32(gid), st.Gid)
}

func TestPutWithoutAttributesUsesDefaultPermissions(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(t, root)

	rr := serve(handler, newChunk("file", 0, 4, "data"))
	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())

	fi, err := os.Lstat(filepath.Join(root, "file"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), fi.Mode())
}

func TestPutWithInvalidHeadersIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{name: "non-octal permissions", headers: map[string]string{"X-Attribute-Permissions": "0999"}},
		{name: "permissions with special bits", headers: map[string]string{"X-Attribute-Permissions": "4755"}},
		{name: "non-numeric uid", headers: map[string]string{"X-Attribute-Uid": "root"}},
		{name: "negative gid", headers: map[string]string{"X-Attribute-Gid": "-1"}},
		{name: "invalid mod time", headers: map[string]string{"X-Attribute-ModTime": "yesterday"}},
		{name: "negative offset", headers: map[string]string{"X-Offset": "-1"}},
		{name: "non-numeric total", headers: map[string]string{"X-Content-Length": "all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			handler := newHandler(t, root)
			req := newChunk("file", 0, 4, "data")
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}

			rr := serve(handler, req)

			require.Equal(t, http.StatusBadRequest, rr.Code)
			requireRootEmpty(t, root)
		})
	}
}

func TestConcurrentPutIsRefused(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(t, root)

	pr, pw := io.Pipe()
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		done <- serve(handler, newChunkRequest("first", 0, 8, pr))
	}()

	// The pipe hands the bytes over only once the first PUT reads them, so after this write
	// the first PUT is in the middle of copying its body.
	_, err := pw.Write([]byte("0123"))
	require.NoError(t, err)

	rr := serve(handler, newChunk("second", 0, 4, "data"))
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Empty(t, rr.Header().Get("X-Expected-Offset"))
	requireNotExist(t, filepath.Join(root, "second.tempupload"))

	_, err = pw.Write([]byte("4567"))
	require.NoError(t, err)
	require.NoError(t, pw.Close())
	rr = <-done
	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
	requireFileContent(t, filepath.Join(root, "first"), "01234567")

	require.Equal(t, http.StatusCreated, serve(handler, newChunk("second", 0, 4, "data")).Code, "a finished PUT must release the handler")
}

func TestPutFailsWhenFileCannotBeWritten(t *testing.T) {
	t.Run("parent is a file", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "parent"), []byte("data"), 0o640))
		handler := newHandler(t, root)

		rr := serve(handler, newChunk("parent/file", 0, 4, "data"))

		require.Equal(t, http.StatusInternalServerError, rr.Code)
		requireFileContent(t, filepath.Join(root, "parent"), "data")
	})

	t.Run("file name too long", func(t *testing.T) {
		root := t.TempDir()
		handler := newHandler(t, root)

		rr := serve(handler, newChunk(strings.Repeat("n", 300), 0, 4, "data"))

		require.Equal(t, http.StatusInternalServerError, rr.Code)
		requireRootEmpty(t, root)
	})

	t.Run("temporary file name too long", func(t *testing.T) {
		root := t.TempDir()
		handler := newHandler(t, root)

		rr := serve(handler, newChunk(strings.Repeat("n", 250), 0, 4, "data"))

		require.Equal(t, http.StatusInternalServerError, rr.Code)
		requireRootEmpty(t, root)
	})

	t.Run("temporary file is a directory", func(t *testing.T) {
		root := t.TempDir()
		temp := filepath.Join(root, "file.tempupload")
		require.NoError(t, os.Mkdir(temp, 0o750))
		fi, err := os.Lstat(temp)
		require.NoError(t, err)
		handler := newHandler(t, root)

		rr := serve(handler, newChunk("file", int(fi.Size()), int(fi.Size())+4, "data"))

		require.Equal(t, http.StatusInternalServerError, rr.Code)
		requireNotExist(t, filepath.Join(root, "file"))
	})

	t.Run("request body fails", func(t *testing.T) {
		root := t.TempDir()
		handler := newHandler(t, root)

		rr := serve(handler, newChunkRequest("file", 0, 4, iotest.ErrReader(errInjected)))

		require.Equal(t, http.StatusInternalServerError, rr.Code)
		requireNotExist(t, filepath.Join(root, "file"))
	})
}

func TestPostMarksImportFinished(t *testing.T) {
	repo := newClient(t, common.ServerStateReady, interceptor.Funcs{})
	handler := newHandlerWithClient(t, t.TempDir(), repo)

	rr := serve(handler, newRequest(http.MethodPost, ""))

	require.Equal(t, http.StatusOK, rr.Code, "body=%q", rr.Body.String())
	di := &api.DataImport{}
	require.NoError(t, repo.Get(context.Background(), types.NamespacedName{Namespace: "project", Name: "import"}, di))
	require.Equal(t, string(common.ServerStateFinished), di.Status.ServerState)
}

func TestPostFailsWhenStateCannotBeSaved(t *testing.T) {
	repo := newClient(t, common.ServerStateReady, interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
			return errInjected
		},
	})
	handler := newHandlerWithClient(t, t.TempDir(), repo)

	rr := serve(handler, newRequest(http.MethodPost, ""))

	require.Equal(t, http.StatusInternalServerError, rr.Code)
	di := &api.DataImport{}
	require.NoError(t, repo.Get(context.Background(), types.NamespacedName{Namespace: "project", Name: "import"}, di))
	require.Equal(t, string(common.ServerStateReady), di.Status.ServerState)
}

func TestPutReportsFailedRenameOfCompletedUpload(t *testing.T) {
	root := t.TempDir()
	// A directory in place of the final file makes the rename of the completed upload fail.
	final := filepath.Join(root, "file")
	require.NoError(t, os.MkdirAll(filepath.Join(final, "child"), 0o750))
	require.NoError(t, os.WriteFile(final+".tempupload", []byte("da"), 0o640))
	handler := newHandler(t, root)

	rr := serve(handler, newChunk("file", 2, 4, "ta"))

	require.Equal(t, http.StatusInternalServerError, rr.Code, "body=%q", rr.Body.String())
	fi, err := os.Lstat(final)
	require.NoError(t, err)
	require.True(t, fi.IsDir())
	requireFileContent(t, final+".tempupload", "data")
}

func TestPutOfPartialChunkAnswersWithoutError(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(t, root)
	req := newChunk("file", 0, 10, "01234")
	req.Header.Set("X-Attribute-Permissions", "0604")
	req.Header.Set("X-Attribute-ModTime", "2025-06-07T08:09:10Z")

	rr := serve(handler, req)

	require.Equal(t, http.StatusNoContent, rr.Code)
	require.Empty(t, rr.Body.String())
	requireNotExist(t, filepath.Join(root, "file"))
}

func TestPutOfPartialChunkLeavesExistingFileAlone(t *testing.T) {
	root := t.TempDir()
	final := filepath.Join(root, "file")
	mtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, os.WriteFile(final, []byte("old"), 0o600))
	require.NoError(t, os.Chmod(final, 0o600))
	require.NoError(t, os.Chtimes(final, time.Time{}, mtime))
	handler := newHandler(t, root)
	req := newChunk("file", 0, 10, "01234")
	req.Header.Set("X-Attribute-Permissions", "0644")
	req.Header.Set("X-Attribute-ModTime", "2025-06-07T08:09:10Z")

	rr := serve(handler, req)

	require.Equal(t, http.StatusNoContent, rr.Code, "body=%q", rr.Body.String())
	fi, err := os.Lstat(final)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode())
	require.True(t, mtime.Equal(fi.ModTime()), "want %v, got %v", mtime, fi.ModTime())
	requireFileContent(t, final, "old")
}

func TestPutAppliesAttributesOfLastChunk(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(t, root)
	first := newChunk("file", 0, 8, "0123")
	first.Header.Set("X-Attribute-Permissions", "0600")
	require.Equal(t, http.StatusNoContent, serve(handler, first).Code)
	last := newChunk("file", 4, 8, "4567")
	last.Header.Set("X-Attribute-Permissions", "0604")

	rr := serve(handler, last)

	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
	fi, err := os.Lstat(filepath.Join(root, "file"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o604), fi.Mode())
}

func TestPutReportsFailedAttributesOfCompletedUpload(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may change the owner of a file to any user")
	}
	root := t.TempDir()
	handler := newHandler(t, root)
	req := newChunk("file", 0, 4, "data")
	req.Header.Set("X-Attribute-Uid", "4321")
	req.Header.Set("X-Attribute-Gid", "8765")

	rr := serve(handler, req)

	require.Equal(t, http.StatusInternalServerError, rr.Code, "body=%q", rr.Body.String())
	requireNotExist(t, filepath.Join(root, "file"))
	requireFileContent(t, filepath.Join(root, "file.tempupload"), "data")
}

func TestPutReplacesExistingFileOfOtherSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  string
	}{
		{name: "smaller", old: "old"},
		{name: "larger", old: "old content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			final := filepath.Join(root, "file")
			require.NoError(t, os.WriteFile(final, []byte(tc.old), 0o640))
			handler := newHandler(t, root)

			rr := serve(handler, newChunk("file", 0, 10, "01234"))
			require.Equal(t, http.StatusNoContent, rr.Code, "body=%q", rr.Body.String())
			require.Equal(t, "5", rr.Header().Get("X-Next-Offset"))

			rr = serve(handler, newChunk("file", 5, 10, "56789"))
			require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
			requireFileContent(t, final, "0123456789")
			requireNotExist(t, final+".tempupload")
		})
	}
}
