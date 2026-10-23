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
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

func newHandler(t *testing.T, root string) http.Handler {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	cfg := &config.Config{
		Mode:            config.VolumeModeFilesystem,
		Operation:       common.OperationImport,
		DataManagerName: "import",
		URLOpt:          config.URLOpt{DataManagerNamespace: "project", DataManagerTargetKindShort: "pvc", DataManagerTargetName: "volume"},
	}
	client := &repository.Client{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&api.DataImport{ObjectMeta: metav1.ObjectMeta{Namespace: "project", Name: "import"}}).
		Build()}

	handler, err := importfilesystem.NewImportFilesystemHandler(real.GetOS(), root, slog.New(slog.DiscardHandler), client, cfg, client)
	require.NoError(t, err)

	return handler
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
