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
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/deckhouse/sds-common-lib/fs/mockfs"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/authorization"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/mock"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/test"
)

func createFsFuzzTest(t assert.TestingT) *mockfs.MockFS {
	fsys, _ := mockfs.NewFsMock()

	// FS mock
	// /
	// ├── block
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

	block, err := fsys.CreateFile("/block", os.ModeDevice)
	assert.NoError(t, err)
	test.SetContent(block, "fake_device_content")

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

func createRequest(
	t *testing.T,
	authType string,
	basicCreds string,
	bearerToken string,
	wrongAuth string,
	rawURL string,
	methodIdx int,
	body []byte,
) *http.Request {
	// Map method index to a broad set of methods (includes unsupported ones)
	methods := []string{
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

	if methodIdx < 0 {
		methodIdx = -methodIdx
	}
	method := methods[methodIdx%len(methods)]

	// Create request using http.NewRequest to avoid builder panics
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Skip()
	}

	// Set Authorization header based on fuzzed auth type
	switch strings.ToLower(authType) {
	case "basic":
		encoded := base64.StdEncoding.EncodeToString([]byte(basicCreds))
		req.Header.Set("Authorization", "Basic "+encoded)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	default:
		req.Header.Set("Authorization", wrongAuth)
	}

	return req
}

// FuzzHTTPServer fuzzes the HTTP server endpoints using go-fuzz-headers.
func FuzzHandler(f *testing.F) {
	// Seed corpus with a few URLs and independent data bytes
	// fs
	// regular cases (correct prefix, auth, URL)
	f.Add("bearer", "", "token-123", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/file", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/file", 1, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/file", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/file?attribute=stat&attribute=hash.md5", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/file/", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/bad_file", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/file_link", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir_link", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir_link", 1, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/", 1, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/dir/", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/?attribute=stat&attribute=hash.md5", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_file", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_file", 1, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_file?attribute=hash.md5", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "d", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_link", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_dir/", 0, []byte("12345"), int64(1234), byte(5))
	f.Add("bearer", "", "token-123", "", "https://foo.bar/api/v1/files/dir/nested_dir", 0, []byte("12345"), int64(1234), byte(5))
	// random cases
	f.Add("basic", "user:pass", "", "", "https://foo.bar/api/v1/files/", 0, []byte("12345"), int64(174757), byte(20))
	f.Add("bearer", "", "dXNlcjpwYXNzd2Q=", "", "https://foo.bar/hello/v1/files/dir/?attribute=stat", 1, []byte("hello"), int64(573558), byte(0))
	f.Add("wrong", "", "", " abcdйцукен///⚺⪣⾓␜ⅷⱦ⣠ⵉ⾛⃚⴦ⶭ", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/dir_link?attribute=stat&attribute=hash.md5", 2, []byte("qwerty"), int64(88478), byte(255))
	f.Add("basic", "user:pass", "", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/file/", 3, []byte("284756"), int64(8474), byte(100))
	f.Add("bearer", "", "113edec49eaa=", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/../../foo", 0, []byte("84831"), int64(5875), byte(50))
	f.Add("bearer", "", "⡋⃤⣿⋎⭔┛⺗⏯╽", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/files/ abcdйцукен///⚺⪣⾓␜ⅷⱦ⣠ⵉ⾛⃚⴦ⶭ", 0, []byte("48575"), int64(1234), byte(10))
	// block
	// regular cases
	f.Add("bearer", "", "some-token", "", "https://foo.bar/api/v1/block", 0, []byte("12345"), int64(4567), byte(5))
	f.Add("bearer", "", "some-token", "", "https://foo.bar/api/v1/block", 1, []byte("12345"), int64(4567), byte(5))
	f.Add("bearer", "", "some-token", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/block", 0, []byte("12345"), int64(4567), byte(5))
	f.Add("bearer", "", "some-token", "", "https://foo.bar/api/v1/block/", 0, []byte("12345"), int64(4567), byte(5))
	f.Add("bearer", "", "some-token", "", "https://foo.bar/api/v1/block/file", 0, []byte("12345"), int64(4567), byte(5))
	// random cases
	f.Add("wrong", "", "", "somerandom", "https://foo.bar/de-ns/de-kind/de-name/api/v1/block", 1, []byte("hello"), int64(2345), byte(4))
	f.Add("basic", "user:password", "", "", "https://foo.bar/api/v1/block/file", 0, []byte("12345"), int64(174757), byte(10))
	f.Add("bearer", "", "113edec49eaa=", "", "https://foo.bar/dir/de-ns/de-kind/de-name/api/v1/block", 1, []byte("hello"), int64(434), byte(0))
	f.Add("bearer", "", "FMfjh84frk=", "", "https://foo.bar/de-ns/de-kind/de-name/api/v1/block/☰∉Ⅿ⏵⡪↿⊡/../⪭⬆⮖Ⰶ⭆⫳✨", 1, []byte("hello"), int64(434), byte(0))

	opt := config.URLOpt{
		DataManagerNamespace:       "de-ns",
		DataManagerTargetKindShort: "de-kind",
		DataManagerTargetName:      "de-name",
	}

	fsys := createFsFuzzTest(f)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	exportFS, err := NewExporterHandler(fsys, "/sharedir", config.VolumeModeFilesystem, logger)
	assert.NoError(f, err)

	exportBlock, err := NewExporterHandler(fsys, "/block", config.VolumeModeBlock, logger)
	assert.NoError(f, err)

	f.Fuzz(func(t *testing.T, authType string, basicCreds string, bearerToken string, wrongAuth string, rawURL string, methodIdx int, body []byte, failSeed int64, failProb byte) {
		// Configure failure injection
		probability := float64(failProb) / 255.0
		fsys.Failer = mockfs.NewProbabilityFailer(failSeed, probability)

		ctrl := gomock.NewController(t)
		k8sClient := mock.NewMockUserAuthorizer(ctrl)
		k8sClient.
			EXPECT().
			AuthenticateUserByToken(gomock.Any(), gomock.Any()).
			Return(true, "user", []string{"group"}, nil).
			AnyTimes()

		k8sClient.
			EXPECT().
			AuthorizeUser(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(true, "", nil).
			AnyTimes()

		auth := func(handler http.Handler) http.Handler {
			return authorization.Authorize(
				handler,
				k8sClient,
				common.OperationExport,
				opt.DataManagerNamespace,
			)
		}

		{
			// handle fs request
			mux := http.NewServeMux()
			handler := auth(exportFS)
			MuxAddFSHandler(mux, opt, handler, logger)

			req := createRequest(t, authType, basicCreds, bearerToken, wrongAuth, rawURL, methodIdx, body)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
		}

		{
			// handle block request
			mux := http.NewServeMux()
			handler := auth(exportBlock)
			MuxAddBlockHandler(mux, opt, handler, logger)

			req := createRequest(t, authType, basicCreds, bearerToken, wrongAuth, rawURL, methodIdx, body)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
		}
	})
}
