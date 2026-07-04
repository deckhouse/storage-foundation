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

package export_block_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/deckhouse/sds-common-lib/fs/mockfs"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/export_block"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/test"
)

func createTestFs(t *testing.T) *mockfs.MockFS {
	fsys, _ := mockfs.NewFsMock()

	// /
	// ├── good (char device)
	// ├── dir (directory)
	//     └── good_nested (char device)
	// └── bad  (regular file)

	good, err := fsys.CreateFile("/good", os.ModeDevice)
	assert.NoError(t, err)
	test.SetContent(good, "good_device_content")

	_, err = fsys.CreateFile("/bad", 0o664)
	assert.NoError(t, err)

	_, err = fsys.CreateFile("/dir", os.ModeDir)
	assert.NoError(t, err)

	goodNested, err := fsys.CreateFile("/dir/good_nested", os.ModeDevice)
	test.SetContent(goodNested, "good_nested_device_content")
	assert.NoError(t, err)

	return fsys
}

func newBlockHandler(fsys *mockfs.MockFS, path string, logger *slog.Logger) (*http.ServeMux, error) {
	blockExporter, err := export_block.NewBlockExporter(fsys, path, logger)
	if err != nil {
		return nil, err
	}

	// We can't create request with empty path but we need it in BlockExporter
	mux := http.NewServeMux()

	// TODO: Copied from main. Move to separate function
	handler := blockExporter
	mux.Handle("/api/v1/block", http.StripPrefix("/api/v1/block", handler))

	wrongPrefixFilesHandler := http.HandlerFunc(export_block.WrongPrefixFilesInformer)
	mux.Handle("/api/v1/files", wrongPrefixFilesHandler)
	mux.Handle("/api/v1/files/", wrongPrefixFilesHandler)

	wrongPrefixBlockHandler := http.HandlerFunc(export_block.WrongPrefixBlockInformer)
	mux.Handle("/api/v1/block/", wrongPrefixBlockHandler)

	return mux, nil
}

func serve(t *testing.T, fsys *mockfs.MockFS, w http.ResponseWriter, r *http.Request) {
	logger := newLogger()
	e, err := newBlockHandler(fsys, "/good", logger)
	assert.NoError(t, err)
	e.ServeHTTP(w, r)
}

func serveNested(t *testing.T, fsys *mockfs.MockFS, w http.ResponseWriter, r *http.Request) {
	logger := newLogger()
	e, err := newBlockHandler(fsys, "/dir/good_nested", logger)
	assert.NoError(t, err)
	e.ServeHTTP(w, r)
}

func assertCommonHeaders(t *testing.T, response *http.Response, expectedSize int64) {
	test.AssertHeader(t, response, "Content-Type", "application/octet-stream")
	test.AssertHeader(t, response, "Content-Length", fmt.Sprint(expectedSize))
	test.AssertHeader(t, response, "Content-Disposition", "attachment; filename=data.img")
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

// Positive: init with block device
func TestInitGoodDevice(t *testing.T) {
	fsys := createTestFs(t)
	logger := newLogger()

	e, err := newBlockHandler(fsys, "/good", logger)
	assert.NoError(t, err)
	assert.NotNil(t, e)
}
func TestInitGoodNestedDevice(t *testing.T) {
	fsys := createTestFs(t)
	logger := newLogger()

	e, err := newBlockHandler(fsys, "/dir/good_nested", logger)
	assert.NoError(t, err)
	assert.NotNil(t, e)
}

// Negative: init with file which is not a block device
func TestInitBadDevice(t *testing.T) {
	fsys := createTestFs(t)
	logger := newLogger()

	e, err := newBlockHandler(fsys, "/bad", logger)
	assert.Error(t, err)
	assert.Nil(t, e)
}

// Negative: wrong HTTP method
func TestWrongHttpMethod(t *testing.T) {
	fsys := createTestFs(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/block", http.NoBody)
	rr := httptest.NewRecorder()

	serve(t, fsys, rr, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

// Positive: request file
func TestHeadOK(t *testing.T) {
	fsys := createTestFs(t)

	req := httptest.NewRequest(http.MethodHead, "/api/v1/block", http.NoBody)
	rr := httptest.NewRecorder()

	serve(t, fsys, rr, req)

	res := rr.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	good, _ := fsys.GetFile("/good")
	assertCommonHeaders(t, res, good.Size)
}

func TestNestedHeadOK(t *testing.T) {
	fsys := createTestFs(t)

	req := httptest.NewRequest(http.MethodHead, "/api/v1/block", http.NoBody)
	rr := httptest.NewRecorder()

	serveNested(t, fsys, rr, req)

	res := rr.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	good, _ := fsys.GetFile("/dir/good_nested")
	assertCommonHeaders(t, res, good.Size)
}

func TestNestedGetOK(t *testing.T) {
	fsys := createTestFs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/block", http.NoBody)
	rr := httptest.NewRecorder()

	serveNested(t, fsys, rr, req)

	res := rr.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	good, _ := fsys.GetFile("/dir/good_nested")
	assertCommonHeaders(t, res, good.Size)
}
func TestGetOK(t *testing.T) {
	fsys := createTestFs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/block", http.NoBody)
	rr := httptest.NewRecorder()

	serve(t, fsys, rr, req)

	res := rr.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	good, _ := fsys.GetFile("/good")
	assertCommonHeaders(t, res, good.Size)
}

func TestHeadWrongPath(t *testing.T) {
	fsys := createTestFs(t)

	req := httptest.NewRequest(http.MethodHead, "/api/v1/block/some", http.NoBody)
	rr := httptest.NewRecorder()

	serve(t, fsys, rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGetWrongPath(t *testing.T) {
	fsys := createTestFs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/block/some", http.NoBody)
	rr := httptest.NewRecorder()

	serve(t, fsys, rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}
