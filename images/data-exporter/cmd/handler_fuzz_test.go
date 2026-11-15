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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/deckhouse/sds-common-lib/fs/failer"
	"github.com/deckhouse/sds-common-lib/fs/fake"
	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/authorization"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/mock"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/ttl_control"
)

func createFsFuzzTest(t assert.TestingT) *fake.OS {
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
	fsys, err := fake.NewBuilder("/").
		WithFileAtPath("block", os.ModeDevice, fake.RWContentFromString("fake_device_content")).
		WithFileAtPath("sharedir", os.ModeDir).
		WithFileAtPath("sharedir/dir", os.ModeDir).
		WithFileAtPath("sharedir/dir/nested_dir", os.ModeDir|0o750).
		WithFileAtPath("sharedir/dir/nested_file", os.FileMode(0o664), fake.RWContentFromString("nested_file")).
		WithFileAtPath("sharedir/dir/nested_link", os.ModeSymlink|0o664, fake.LinkReader{Target: "../file"}).
		WithFileAtPath("sharedir/dir_link", os.ModeSymlink, fake.LinkReader{Target: "/outer_dir"}).
		WithFileAtPath("sharedir/file", os.FileMode(0o664), fake.RWContentFromString("file")).
		WithFileAtPath("sharedir/file_link", os.ModeSymlink, fake.LinkReader{Target: "/foo/bar"}).
		WithFileAtPath("outer_dir", os.ModeDir).
		WithFileAtPath("outer_dir/file", os.FileMode(0o664)).
		WithFileAtPath("outer_file", os.FileMode(0o664)).
		Build()
	assert.NoError(t, err)

	// The old mock applied one DefaultSys to every file; ownership is per entry now.
	for _, path := range []string{
		"block",
		"sharedir", "sharedir/dir", "sharedir/dir/nested_dir", "sharedir/dir/nested_file",
		"sharedir/dir/nested_link", "sharedir/dir_link", "sharedir/file", "sharedir/file_link",
		"outer_dir", "outer_dir/file", "outer_file",
	} {
		assert.NoError(t, fsys.Chown(path, fuzzUID, fuzzGID))
	}

	return fsys
}

const (
	fuzzUID = 456
	fuzzGID = 123
)

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
	logger := logger.NewNop()

	f.Fuzz(func(t *testing.T, authType string, basicCreds string, bearerToken string, wrongAuth string, rawURL string, methodIdx int, body []byte, failSeed int64, failProb byte) {
		iteration := func(t *testing.T) {
			// Failure injection is a decorator around the filesystem now, not a field on the
			// mock, so the handlers are constructed per iteration on top of the wrapped one.
			// Construction itself touches the filesystem, so an injected failure there is a
			// legitimate outcome and ends the iteration rather than failing the test.
			probability := float64(failProb) / 255.0
			failing := failer.NewOS(fsys, failer.NewProbabilityFailer(failSeed, probability))

			exportFS, err := NewExporterHandler(failing, "/sharedir", config.VolumeModeFilesystem, logger)
			if err != nil {
				return
			}

			exportBlock, err := NewExporterHandler(failing, "/block", config.VolumeModeBlock, logger)
			if err != nil {
				return
			}

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

			updater := mock.NewMockDataManagerAccessTimestampUpdater(ctrl)
			updater.EXPECT().UpdateAccessTimestamp(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			setter := mock.NewMockDataManagerServerStateSetter(ctrl)
			setter.EXPECT().SetServerState(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			ttlControl, err := ttl_control.NewTTLControl(common.OperationExport, "1m", opt.DataManagerNamespace, opt.DataManagerTargetName, updater, setter, logger)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			ttlControl.Start(ctx, &wg)
			defer func() {
				time.Sleep(3 * time.Minute)
				synctest.Wait()
				cancel()
				wg.Wait()
			}()

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
				handler := auth(ttlControl.Wrap(exportFS))
				MuxAddFSHandler(mux, opt, handler, logger)

				req := createRequest(t, authType, basicCreds, bearerToken, wrongAuth, rawURL, methodIdx, body)
				rr := httptest.NewRecorder()
				mux.ServeHTTP(rr, req)
			}

			{
				// handle block request
				mux := http.NewServeMux()
				handler := auth(ttlControl.Wrap(exportBlock))
				MuxAddBlockHandler(mux, opt, handler, logger)

				req := createRequest(t, authType, basicCreds, bearerToken, wrongAuth, rawURL, methodIdx, body)
				rr := httptest.NewRecorder()
				mux.ServeHTTP(rr, req)
			}
		}
		synctest.Test(t, iteration)
	})
}
