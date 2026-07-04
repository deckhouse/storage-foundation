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

package export_filesystem_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/deckhouse/sds-common-lib/fs/fsext"
	"github.com/deckhouse/sds-common-lib/fs/mockfs"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/export_filesystem"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/test"
)

func createTestFs(t *testing.T) *mockfs.MockFS {
	fsys, _ := mockfs.NewFsMock()

	// FS mock
	// /
	// ├── sharedir <- share this
	// │   ├── dir
	// │   │   ├── nested_dir (empty)
	// │   │   ├── nested_file
	// │   │   └── nested_link -> ../file
	// │   ├── dir_link -> /outer_dir
	// │   ├── file
	// │   └── file_link -> /foo/bar
	// ├── outer_dir
	// │   └── file
	// └── outer_file

	const gid uint32 = 123
	const uid uint32 = 456

	fsys.DefaultSys.Gid = gid
	fsys.DefaultSys.Uid = uid

	var err error
	_, err = fsys.CreateFile("/sharedir", os.ModeDir)
	assert.NoError(t, err)

	_, err = fsys.CreateFile("/sharedir/dir", os.ModeDir)
	assert.NoError(t, err)

	_, err = fsys.CreateFile("/sharedir/dir/nested_dir", os.ModeDir|0o750)
	assert.NoError(t, err)

	nestedFile, err := fsys.CreateFile("/sharedir/dir/nested_file", 0o664)
	assert.NoError(t, err)
	test.SetContent(nestedFile, "nested_file")

	nestedLink, err := fsys.CreateFile("/sharedir/dir/nested_link", os.ModeSymlink|0o664)
	assert.NoError(t, err)
	nestedLink.LinkSource = "../file"

	dirLink, err := fsys.CreateFile("/sharedir/dir_link", os.ModeSymlink)
	assert.NoError(t, err)
	dirLink.LinkSource = "/outer_dir"

	file, err := fsys.CreateFile("/sharedir/file", 0o664)
	assert.NoError(t, err)
	test.SetContent(file, "file")

	fileLink, err := fsys.CreateFile("/sharedir/file_link", os.ModeSymlink)
	assert.NoError(t, err)
	fileLink.LinkSource = "/foo/bar"

	_, err = fsys.CreateFile("/outer_dir", os.ModeDir)
	assert.NoError(t, err)

	_, err = fsys.CreateFile("/outer_dir/file", 0o664)
	assert.NoError(t, err)

	_, err = fsys.CreateFile("/outer_file", 0o664)
	assert.NoError(t, err)

	return fsys
}

func getExpectedDirectoryList(fsys *mockfs.MockFS) []export_filesystem.Item {
	nestedDir, _ := fsys.GetFile("/sharedir/dir/nested_dir")
	nestedFile, _ := fsys.GetFile("/sharedir/dir/nested_file")
	nestedLink, _ := fsys.GetFile("/sharedir/dir/nested_link")

	return []export_filesystem.Item{
		{
			Name: "nested_dir",
			Type: "dir",
			URI:  "dir/nested_dir/",
			Attributes: map[string]any{
				"modtime":     nestedDir.ModTime.Format(time.RFC3339),
				"gid":         float64(123), // go unmarshals number to  the interface value as float64
				"uid":         float64(456),
				"permissions": fmt.Sprintf("%#o", uint32(nestedDir.Mode.Perm())),
			},
		},
		{
			Name: "nested_file",
			Type: "file",
			URI:  "dir/nested_file",
			Attributes: map[string]any{
				"modtime":     nestedFile.ModTime.Format(time.RFC3339),
				"size":        float64(nestedFile.Size),
				"gid":         float64(123),
				"uid":         float64(456),
				"permissions": fmt.Sprintf("%#o", uint32(nestedFile.Mode.Perm())),
			},
		},
		{
			Name:       "nested_link",
			Type:       "link",
			URI:        "dir/nested_link",
			TargetPath: "../file",
			Attributes: map[string]any{
				"modtime":     nestedLink.ModTime.Format(time.RFC3339),
				"gid":         float64(123),
				"uid":         float64(456),
				"permissions": fmt.Sprintf("%#o", uint32(nestedLink.Mode.Perm())),
			},
		},
	}
}

func assertFileAttributes(t *testing.T, response *http.Response, fsys *mockfs.MockFS, path string) *mockfs.MockFile {
	file, err := fsys.GetFile(path)
	assert.NoError(t, err)

	test.AssertHeader(t, response, "X-Attribute-Modtime", file.ModTime.Format(time.RFC3339))
	test.AssertHeader(t, response, "X-Attribute-Gid", fmt.Sprint(file.Sys.Gid))
	test.AssertHeader(t, response, "X-Attribute-Uid", fmt.Sprint(file.Sys.Uid))
	test.AssertHeader(t, response, "X-Attribute-Permissions", fmt.Sprintf("%#o", uint32(file.Mode.Perm()))) // octal format

	return file
}

func serve(t *testing.T, fsys fsext.FS, w http.ResponseWriter, r *http.Request) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	d, err := export_filesystem.NewFilesystemExporter(fsys, "/sharedir", logger)
	assert.NoError(t, err)
	d.ServeHTTP(w, r)
}

func readAll(reader io.Reader) ([]byte, error) {
	result := []byte{}
	buffer := make([]byte, 4096)

	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			result = append(result, buffer[:n]...)
		}
		if err == io.EOF {
			break
		} else if err != nil {
			return result, err
		}
	}

	return result, nil
}

func assertDir(t *testing.T, expected []export_filesystem.Item, actual io.Reader) {
	buffer, err := readAll(actual)
	assert.NoError(t, err)

	var actualDir export_filesystem.Response
	err = json.Unmarshal(buffer, &actualDir)
	assert.NoError(t, err)

	assert.Equal(t, expected, actualDir.Items)
}

func TestInitNoSuchDir(t *testing.T) {
	fsys := createTestFs(t)
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	d, err := export_filesystem.NewFilesystemExporter(fsys, "/baddir", logger)
	assert.Error(t, err)
	assert.Nil(t, d)
}

func TestInitNotADir(t *testing.T) {
	fsys := createTestFs(t)
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	d, err := export_filesystem.NewFilesystemExporter(fsys, "/outer_file", logger)
	assert.Error(t, err)
	assert.Nil(t, d)
}

func TestWrongHttpMethod(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodPost, "/file", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

// Positive: request file
func TestHeadFile(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodHead, "/file", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
	file := assertFileAttributes(t, result, fsys, "/sharedir/file")
	test.AssertHeader(t, result, "Content-Length", fmt.Sprint(file.Size))
	test.AssertHeader(t, result, "Content-Type", "application/octet-stream")
	test.AssertHeader(t, result, "Content-Disposition", "attachment; filename=file")
	test.AssertHeader(t, result, "X-Type", "file")
	test.AssertNoHeader(t, result, "X-Linktarget")
	test.AssertNoHeader(t, result, "X-Attribute-Hash-Md5")
}

// Positive: request file
func TestGetFile(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodGet, "/file", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
	file := assertFileAttributes(t, result, fsys, "/sharedir/file")
	test.AssertHeader(t, result, "Content-Length", fmt.Sprint(file.Size))
	test.AssertHeader(t, result, "Content-Type", "application/octet-stream")
	test.AssertHeader(t, result, "Content-Disposition", "attachment; filename=file")
	test.AssertHeader(t, result, "X-Type", "file")
	test.AssertNoHeader(t, result, "X-Linktarget")
	test.AssertNoHeader(t, result, "X-Attribute-Hash-Md5")

	test.AssertFileContent(t, result.Body, []byte("file"))
}

// Positive: request file
func TestGetNestedFile(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodGet, "/dir/nested_file", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
	file := assertFileAttributes(t, result, fsys, "/sharedir/dir/nested_file")
	test.AssertHeader(t, result, "Content-Length", fmt.Sprint(file.Size))
	test.AssertHeader(t, result, "Content-Type", "application/octet-stream")
	test.AssertHeader(t, result, "Content-Disposition", "attachment; filename=nested_file")
	test.AssertHeader(t, result, "X-Type", "file")
	test.AssertNoHeader(t, result, "X-Linktarget")
	test.AssertNoHeader(t, result, "X-Attribute-Hash-Md5")

	test.AssertFileContent(t, result.Body, []byte("nested_file"))
}

// Positive: request md5 sum of the file
func TestHeadFileMd5(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodHead, "/file?attribute=stat&attribute=hash.md5", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
	file := assertFileAttributes(t, result, fsys, "/sharedir/file")
	test.AssertHeader(t, result, "Content-Length", fmt.Sprint(file.Size))
	test.AssertHeader(t, result, "Content-Type", "application/octet-stream")
	test.AssertHeader(t, result, "Content-Disposition", "attachment; filename=file")
	test.AssertHeader(t, result, "X-Type", "file")
	test.AssertHeader(t, result, "X-Attribute-Hash-Md5", "8c7dd922ad47494fc02c388e12c00eac") // echo -n "file" | md5sum
	test.AssertNoHeader(t, result, "X-Linktarget")
}

// Negative: request file with trailing /
func TestHeadFileTrailingSlash(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodHead, "/file/", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusBadRequest, result.StatusCode)
}

// Positive: request directory
func TestHeadDirectory(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodHead, "/dir/", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
	assertFileAttributes(t, result, fsys, "/sharedir/dir")
	test.AssertHeader(t, result, "Content-Type", "application/json")
	test.AssertHeader(t, result, "Transfer-Encoding", "chunked")
	test.AssertHeader(t, result, "X-Type", "dir")
	test.AssertNoHeader(t, result, "Content-Length")
	test.AssertNoHeader(t, result, "Content-Disposition")
	test.AssertNoHeader(t, result, "X-Linktarget")
	test.AssertNoHeader(t, result, "X-Attribute-Hash-Md5")
}

// Positive: request directory
func TestGetDirectory(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodGet, "/dir/", body)
	rr := httptest.NewRecorder()

	expectedDir := getExpectedDirectoryList(fsys)

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
	assertFileAttributes(t, result, fsys, "/sharedir/dir")
	test.AssertHeader(t, result, "Content-Type", "application/json")
	test.AssertHeader(t, result, "Transfer-Encoding", "chunked")
	test.AssertHeader(t, result, "X-Type", "dir")
	test.AssertNoHeader(t, result, "Content-Length")
	test.AssertNoHeader(t, result, "Content-Disposition")
	test.AssertNoHeader(t, result, "X-Linktarget")
	test.AssertNoHeader(t, result, "X-Attribute-Hash-Md5")

	assertDir(t, expectedDir, result.Body)
}

// Positive: request directory with md5 hash
func TestHeadDirectoryMd5(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodGet, "/dir/?attribute=hash.md5", body)
	rr := httptest.NewRecorder()

	expectedDir := getExpectedDirectoryList(fsys)
	// Add md5 attribute to regular file
	expectedDir[1].Attributes["hash.md5"] = "0d7fcc1470cce86180a7d050c95f57e1" // echo -n "nested_file" | md5sum

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
	assertFileAttributes(t, result, fsys, "/sharedir/dir")
	test.AssertHeader(t, result, "Content-Type", "application/json")
	test.AssertHeader(t, result, "Transfer-Encoding", "chunked")
	test.AssertHeader(t, result, "X-Type", "dir")
	test.AssertNoHeader(t, result, "Content-Length")
	test.AssertNoHeader(t, result, "Content-Disposition")
	test.AssertNoHeader(t, result, "X-Linktarget")
	test.AssertNoHeader(t, result, "X-Attribute-Hash-Md5") // dir itself has no md5

	assertDir(t, expectedDir, result.Body)
}

// Negative: request directory without trailing /
func TestHeadDirectoryWithoutSlash(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodHead, "/dir", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusBadRequest, result.StatusCode)
}

// Positive: request symlink
func TestHeadSymlink(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodHead, "/file_link", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
	assertFileAttributes(t, result, fsys, "/sharedir/dir")
	test.AssertHeader(t, result, "Content-Type", "application/x-symlink")
	test.AssertHeader(t, result, "Content-Length", "0")
	test.AssertHeader(t, result, "X-Type", "link")
	test.AssertHeader(t, result, "X-Linktarget", "/foo/bar")
	test.AssertNoHeader(t, result, "Content-Disposition")
	test.AssertNoHeader(t, result, "X-Attribute-Hash-Md5")
}

// Positive: request symlink
func TestGetSymlink(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodGet, "/file_link", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()

	assert.Equal(t, http.StatusOK, result.StatusCode)
	assertFileAttributes(t, result, fsys, "/sharedir/dir")
	test.AssertHeader(t, result, "Content-Type", "application/x-symlink")
	test.AssertHeader(t, result, "Content-Length", "0")
	test.AssertHeader(t, result, "X-Type", "link")
	test.AssertHeader(t, result, "X-Linktarget", "/foo/bar")
	test.AssertNoHeader(t, result, "Content-Disposition")
	test.AssertNoHeader(t, result, "X-Attribute-Hash-Md5")

	test.AssertFileContent(t, result.Body, []byte{})
}

// Negative: file not found
func TestHeadFileNotFound(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodHead, "/some_other_file", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()
	assert.Equal(t, http.StatusNotFound, result.StatusCode)
}

// Negative: access file out of the dir
func TestHeadFileUpPath(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodHead, "/../outer_file", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()
	assert.Equal(t, http.StatusNotFound, result.StatusCode)
}

// Negative: try to access file with symlinks in the path
func TestHeadFileSymlinkInPath(t *testing.T) {
	fsys := createTestFs(t)

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodHead, "/dir_link/file", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify
	result := rr.Result()
	assert.Equal(t, http.StatusBadRequest, result.StatusCode)
}

// Regression: sockets and other special files must appear as "other" in directory listing,
// not as "dir". Before the fix, the default branch in resolveItem used Type: directory,
// causing d8 to treat sockets as directories and fail with 400 on recursive descent.
func TestGetDirectoryWithSocketReturnsOtherType(t *testing.T) {
	fsys, _ := mockfs.NewFsMock()
	fsys.DefaultSys.Gid = 999
	fsys.DefaultSys.Uid = 0

	_, err := fsys.CreateFile("/sharedir", os.ModeDir)
	assert.NoError(t, err)

	_, err = fsys.CreateFile("/sharedir/dir_with_special", os.ModeDir)
	assert.NoError(t, err)

	socket, err := fsys.CreateFile("/sharedir/dir_with_special/execq", os.ModeSocket|0o660)
	assert.NoError(t, err)

	regularFile, err := fsys.CreateFile("/sharedir/dir_with_special/regular.txt", 0o664)
	assert.NoError(t, err)
	test.SetContent(regularFile, "data")

	// HTTP mock
	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodGet, "/dir_with_special/", body)
	rr := httptest.NewRecorder()

	// Run
	serve(t, fsys, rr, req)

	// Verify HTTP status
	result := rr.Result()
	assert.Equal(t, http.StatusOK, result.StatusCode)

	// Parse response
	var resp export_filesystem.Response
	raw, err := readAll(result.Body)
	assert.NoError(t, err)
	assert.NoError(t, json.Unmarshal(raw, &resp))

	// Two items expected: execq (socket) and regular.txt — sorted alphabetically
	assert.Len(t, resp.Items, 2)

	// Socket must be "other", not "dir"
	execq := resp.Items[0]
	assert.Equal(t, "execq", execq.Name)
	assert.Equal(t, "other", execq.Type, "socket must be reported as 'other', not 'dir'")
	assert.NotEqual(t, "dir", execq.Type, "regression: socket must not be reported as 'dir'")
	assert.Equal(t, fmt.Sprintf("%#o", uint32(socket.Mode.Perm())), execq.Attributes["permissions"])

	// Regular file must still be "file"
	reg := resp.Items[1]
	assert.Equal(t, "regular.txt", reg.Name)
	assert.Equal(t, "file", reg.Type)
}

// Regression: direct GET/HEAD request to a socket must return 400,
// not attempt to stream it or treat it as a directory.
func TestGetSocketDirectlyReturns400(t *testing.T) {
	fsys, _ := mockfs.NewFsMock()

	_, err := fsys.CreateFile("/sharedir", os.ModeDir)
	assert.NoError(t, err)

	_, err = fsys.CreateFile("/sharedir/execq", os.ModeSocket|0o660)
	assert.NoError(t, err)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			body := strings.NewReader("")
			req := httptest.NewRequest(method, "/execq", body)
			rr := httptest.NewRecorder()

			serve(t, fsys, rr, req)

			assert.Equal(t, http.StatusBadRequest, rr.Result().StatusCode)
		})
	}
}

// Regression: FIFO (named pipe) must also be reported as "other" in directory listing.
func TestGetDirectoryWithFIFOReturnsOtherType(t *testing.T) {
	fsys, _ := mockfs.NewFsMock()

	_, err := fsys.CreateFile("/sharedir", os.ModeDir)
	assert.NoError(t, err)

	_, err = fsys.CreateFile("/sharedir/alerts", os.ModeDir)
	assert.NoError(t, err)

	_, err = fsys.CreateFile("/sharedir/alerts/cfgarq", os.ModeNamedPipe|0o660)
	assert.NoError(t, err)

	body := strings.NewReader("")
	req := httptest.NewRequest(http.MethodGet, "/alerts/", body)
	rr := httptest.NewRecorder()

	serve(t, fsys, rr, req)

	result := rr.Result()
	assert.Equal(t, http.StatusOK, result.StatusCode)

	var resp export_filesystem.Response
	raw, err := readAll(result.Body)
	assert.NoError(t, err)
	assert.NoError(t, json.Unmarshal(raw, &resp))

	assert.Len(t, resp.Items, 1)
	assert.Equal(t, "cfgarq", resp.Items[0].Name)
	assert.Equal(t, "other", resp.Items[0].Type, "FIFO must be reported as 'other', not 'dir'")
	assert.NotEqual(t, "dir", resp.Items[0].Type, "regression: FIFO must not be reported as 'dir'")
}
