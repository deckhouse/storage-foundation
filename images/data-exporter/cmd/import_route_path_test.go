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

package main

import (
	"io/fs"
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
	"github.com/deckhouse/sds-common-lib/logger"
	api "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/repository"
)

func TestImportFilesRouteKeepsEncodedDotDotInsideRoot(t *testing.T) {
	const outerFileSize = 12345

	cfg := &config.Config{
		Mode:            config.VolumeModeFilesystem,
		Operation:       common.OperationImport,
		DataManagerName: "import",
		URLOpt:          config.URLOpt{DataManagerNamespace: "project", DataManagerTargetKindShort: "pvc", DataManagerTargetName: "volume"},
	}
	logger := logger.NewNop()

	newRouter := func(t *testing.T) (http.Handler, string, string) {
		t.Helper()

		top := t.TempDir()
		root := filepath.Join(top, "volume")
		require.NoError(t, os.Mkdir(root, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(top, "outer_file"), make([]byte, outerFileSize), 0o640))

		scheme := runtime.NewScheme()
		require.NoError(t, api.AddToScheme(scheme))
		client := &repository.Client{Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(&api.DataImport{ObjectMeta: metav1.ObjectMeta{Namespace: "project", Name: "import"}}).
			Build()}

		handler, err := NewImportHandler(real.GetOS(), root, cfg.Mode, logger, client, cfg, client)
		require.NoError(t, err)

		mux := http.NewServeMux()
		MuxAddFSHandler(mux, cfg.URLOpt, handler, logger)

		return mux, top, root
	}

	requireNothingOutsideRoot := func(t *testing.T, top, root string) {
		t.Helper()

		var outside []string
		require.NoError(t, filepath.WalkDir(top, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == root {
				return filepath.SkipDir
			}
			outside = append(outside, strings.TrimPrefix(path, top))
			return nil
		}))
		require.ElementsMatch(t, []string{"", "/outer_file"}, outside)
	}

	for _, prefix := range []string{"/api/v1/files/", GetPathPrefix(cfg.URLOpt) + "api/v1/files/"} {
		for _, escape := range []string{"..%2F", "%2e%2e%2f", "%2E%2E/", "dir%2F..%2F..%2F"} {
			t.Run("PUT "+prefix+escape, func(t *testing.T) {
				router, top, root := newRouter(t)
				req := httptest.NewRequest(http.MethodPut, prefix+escape+"planted", strings.NewReader("planted"))
				req.Header.Set("X-Content-Length", strconv.Itoa(len("planted")))
				req.Header.Set("X-Offset", "0")
				req.Header.Set("X-Attribute-Permissions", "0640")
				req.Header.Set("X-Attribute-Uid", strconv.Itoa(os.Getuid()))
				req.Header.Set("X-Attribute-Gid", strconv.Itoa(os.Getgid()))
				rr := httptest.NewRecorder()

				router.ServeHTTP(rr, req)

				requireNothingOutsideRoot(t, top, root)
				require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
				content, err := os.ReadFile(filepath.Join(root, "planted"))
				require.NoError(t, err)
				require.Equal(t, "planted", string(content))
			})

			t.Run("HEAD "+prefix+escape, func(t *testing.T) {
				router, _, root := newRouter(t)
				require.NoError(t, os.WriteFile(filepath.Join(root, "outer_file"), []byte("inner"), 0o640))
				rr := httptest.NewRecorder()

				router.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, prefix+escape+"outer_file", nil))

				require.Equal(t, http.StatusOK, rr.Code)
				require.Equal(t, strconv.Itoa(len("inner")), rr.Header().Get("Content-Length"))
			})
		}
	}
}
