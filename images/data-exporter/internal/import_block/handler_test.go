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

package importblock_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/sds-common-lib/fs"
	"github.com/deckhouse/sds-common-lib/fs/failer"
	fakefs "github.com/deckhouse/sds-common-lib/fs/fake"
	"github.com/deckhouse/sds-common-lib/logger"
	api "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	importblock "github.com/deckhouse/storage-foundation/images/data-exporter/internal/import_block"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/repository"
)

const (
	devicePath    = "/dev/block"
	deviceContent = "0123456789abcdef"
	importName    = "import"
	importNS      = "project"
)

var errInjected = errors.New("injected failure")

// opFailer fails the filesystem operations its fail function picks; a nil fail lets everything through.
type opFailer struct {
	fail func(op fs.Op, args ...any) error
}

func (f *opFailer) ShouldFail(_ fs.OS, op fs.Op, _ any, args ...any) error {
	if f.fail == nil {
		return nil
	}
	return f.fail(op, args...)
}

type testDevice struct {
	fsys    *fakefs.OS
	content *fakefs.RWContent
}

func newDevice(t *testing.T, content string) testDevice {
	t.Helper()

	rw := fakefs.RWContentFromString(content)
	fsys, err := fakefs.NewBuilder("/").
		WithFileAtPath("dev/block", os.ModeDevice, rw).
		WithFileAtPath("dev/char", os.ModeDevice|os.ModeCharDevice, fakefs.RWContentFromString(content)).
		WithFileAtPath("dev/regular", os.FileMode(0o640), fakefs.RWContentFromString(content)).
		Build()
	require.NoError(t, err)

	return testDevice{fsys: fsys, content: rw}
}

func newClient(t *testing.T, state common.ServerState, funcs interceptor.Funcs) *repository.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	di := &api.DataImport{
		ObjectMeta: metav1.ObjectMeta{Namespace: importNS, Name: importName},
		Status:     api.DataExportImportStatus{ServerState: string(state)},
	}

	return &repository.Client{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(di).
		WithObjects(di).
		WithInterceptorFuncs(funcs).
		Build()}
}

func newConfig() *config.Config {
	return &config.Config{
		Mode:            config.VolumeModeBlock,
		Operation:       common.OperationImport,
		DataManagerName: importName,
		URLOpt:          config.URLOpt{DataManagerNamespace: importNS, DataManagerTargetKindShort: "pvc", DataManagerTargetName: "volume"},
	}
}

func newHandlerWith(t *testing.T, fsys fs.OS, client *repository.Client) *importblock.ImportBlockHandler {
	t.Helper()

	handler, err := importblock.NewImportBlockHandler(fsys, devicePath, logger.NewNop(), client, newConfig(), client)
	require.NoError(t, err)
	require.NotNil(t, handler)

	return handler
}

func newHandler(t *testing.T, dev testDevice) *importblock.ImportBlockHandler {
	t.Helper()
	return newHandlerWith(t, dev.fsys, newClient(t, "", interceptor.Funcs{}))
}

// newRequest builds a request the way it reaches the handler: the route prefix is already stripped,
// so the path is empty.
func newRequest(method string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, "/", body)
	req.URL.Path = ""
	return req
}

func newPut(offset, total int, body string) *http.Request {
	req := newRequest(http.MethodPut, strings.NewReader(body))
	req.Header.Set("X-Offset", strconv.Itoa(offset))
	if total >= 0 {
		req.Header.Set("X-Content-Length", strconv.Itoa(total))
	}
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func requireNextOffset(t *testing.T, h http.Handler, want int) {
	t.Helper()

	rr := serve(h, newRequest(http.MethodHead, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, strconv.Itoa(want), rr.Header().Get("X-Next-Offset"))
}

func TestNewImportBlockHandlerRefusesWhatIsNotAUsableBlockDevice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		content string
		wantErr string
	}{
		{name: "regular file", path: "/dev/regular", content: deviceContent, wantErr: "file is not a block device"},
		{name: "character device", path: "/dev/char", content: deviceContent, wantErr: "file is not a block device"},
		{name: "empty block device", path: devicePath, content: "", wantErr: "bad block device size"},
		{name: "missing path", path: "/dev/missing", content: deviceContent, wantErr: "/dev/missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := newDevice(t, tc.content)
			client := newClient(t, "", interceptor.Funcs{})

			handler, err := importblock.NewImportBlockHandler(dev.fsys, tc.path, logger.NewNop(), client, newConfig(), client)

			require.ErrorContains(t, err, tc.wantErr)
			require.Nil(t, handler)
		})
	}
}

func TestNewImportBlockHandlerFailsWhenDeviceCannotBeRead(t *testing.T) {
	for _, op := range []fs.Op{fs.OpenOp, fs.SeekOp} {
		t.Run(string(op), func(t *testing.T) {
			dev := newDevice(t, deviceContent)
			fsys := failer.NewOS(dev.fsys, &opFailer{fail: func(got fs.Op, _ ...any) error {
				if got == op {
					return errInjected
				}
				return nil
			}})
			client := newClient(t, "", interceptor.Funcs{})

			handler, err := importblock.NewImportBlockHandler(fsys, devicePath, logger.NewNop(), client, newConfig(), client)

			require.ErrorIs(t, err, errInjected)
			require.Nil(t, handler)
		})
	}
}

func TestNewImportBlockHandlerFailsWithoutDataImport(t *testing.T) {
	dev := newDevice(t, deviceContent)
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	client := &repository.Client{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	handler, err := importblock.NewImportBlockHandler(dev.fsys, devicePath, logger.NewNop(), client, newConfig(), client)

	require.True(t, apierrors.IsNotFound(err), "err=%v", err)
	require.Nil(t, handler)
}

func TestServeHTTPRefusesNonEmptyPath(t *testing.T) {
	handler := newHandler(t, newDevice(t, deviceContent))

	for _, method := range []string{http.MethodPut, http.MethodHead, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/file", strings.NewReader("data"))

			rr := serve(handler, req)

			require.Equal(t, http.StatusBadRequest, rr.Code)
		})
	}
}

func TestServeHTTPRefusesEverythingAfterImportFinished(t *testing.T) {
	dev := newDevice(t, deviceContent)
	handler := newHandlerWith(t, dev.fsys, newClient(t, common.ServerStateFinished, interceptor.Funcs{}))

	for _, req := range []*http.Request{
		newPut(0, len(deviceContent), "AAAA"),
		newRequest(http.MethodHead, nil),
		newRequest(http.MethodPost, nil),
	} {
		t.Run(req.Method, func(t *testing.T) {
			rr := serve(handler, req)

			require.Equal(t, http.StatusForbidden, rr.Code)
		})
	}
	require.Equal(t, deviceContent, dev.content.GetString())
}

func TestServeHTTPRefusesUnsupportedMethods(t *testing.T) {
	handler := newHandler(t, newDevice(t, deviceContent))

	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			rr := serve(handler, newRequest(method, nil))

			require.Equal(t, http.StatusMethodNotAllowed, rr.Code)
		})
	}
}

func TestHeadReportsNextOffsetAndDeviceSize(t *testing.T) {
	handler := newHandler(t, newDevice(t, deviceContent))

	rr := serve(handler, newRequest(http.MethodHead, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "0", rr.Header().Get("X-Next-Offset"))
	require.Equal(t, strconv.Itoa(len(deviceContent)), rr.Header().Get("X-Device-Size"))

	require.Equal(t, http.StatusNoContent, serve(handler, newPut(0, len(deviceContent), "AAAAA")).Code)

	rr = serve(handler, newRequest(http.MethodHead, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "5", rr.Header().Get("X-Next-Offset"))
	require.Equal(t, strconv.Itoa(len(deviceContent)), rr.Header().Get("X-Device-Size"))
}

func TestHeadFailsWhenDeviceCannotBeRead(t *testing.T) {
	for _, op := range []fs.Op{fs.OpenOp, fs.SeekOp} {
		t.Run(string(op), func(t *testing.T) {
			f := &opFailer{}
			handler := newHandlerWith(t, failer.NewOS(newDevice(t, deviceContent).fsys, f), newClient(t, "", interceptor.Funcs{}))
			f.fail = func(got fs.Op, _ ...any) error {
				if got == op {
					return errInjected
				}
				return nil
			}

			rr := serve(handler, newRequest(http.MethodHead, nil))

			require.Equal(t, http.StatusInternalServerError, rr.Code)
			require.Empty(t, rr.Header().Get("X-Next-Offset"))
		})
	}
}

func TestPutWritesChunksInOrderAndCompletesOnDeclaredTotal(t *testing.T) {
	dev := newDevice(t, deviceContent)
	handler := newHandler(t, dev)

	rr := serve(handler, newPut(0, len(deviceContent), "AAAA"))
	require.Equal(t, http.StatusNoContent, rr.Code, "body=%q", rr.Body.String())
	require.Equal(t, "4", rr.Header().Get("X-Next-Offset"))
	require.Equal(t, "AAAA456789abcdef", dev.content.GetString())

	rr = serve(handler, newPut(4, len(deviceContent), "BBBBBB"))
	require.Equal(t, http.StatusNoContent, rr.Code, "body=%q", rr.Body.String())
	require.Equal(t, "10", rr.Header().Get("X-Next-Offset"))

	rr = serve(handler, newPut(10, len(deviceContent), "CCCCCC"))
	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
	require.Equal(t, "16", rr.Header().Get("X-Next-Offset"))
	require.Equal(t, "AAAABBBBBBCCCCCC", dev.content.GetString())

	requireNextOffset(t, handler, len(deviceContent))
}

func TestPutCompletesWhenDeclaredTotalIsSmallerThanDevice(t *testing.T) {
	dev := newDevice(t, deviceContent)
	handler := newHandler(t, dev)

	rr := serve(handler, newPut(0, 6, "AAAAAA"))

	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
	require.Equal(t, "6", rr.Header().Get("X-Next-Offset"))
	require.Equal(t, "AAAAAA6789abcdef", dev.content.GetString())
}

func TestPutWithoutDeclaredTotalNeverCompletes(t *testing.T) {
	dev := newDevice(t, deviceContent)
	handler := newHandler(t, dev)

	rr := serve(handler, newPut(0, -1, "AAAAAAAA"))
	require.Equal(t, http.StatusNoContent, rr.Code, "body=%q", rr.Body.String())
	require.Equal(t, "8", rr.Header().Get("X-Next-Offset"))

	rr = serve(handler, newPut(8, -1, "BBBBBBBB"))
	require.Equal(t, http.StatusNoContent, rr.Code, "body=%q", rr.Body.String())
	require.Equal(t, "16", rr.Header().Get("X-Next-Offset"))
	require.Equal(t, "AAAAAAAABBBBBBBB", dev.content.GetString())
}

func TestPutAtWrongOffsetReportsExpectedOffset(t *testing.T) {
	dev := newDevice(t, deviceContent)
	handler := newHandler(t, dev)
	require.Equal(t, http.StatusNoContent, serve(handler, newPut(0, len(deviceContent), "AAAA")).Code)

	for _, offset := range []int{0, 2, 8, len(deviceContent)} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			rr := serve(handler, newPut(offset, len(deviceContent), "ZZZZ"))

			require.Equal(t, http.StatusConflict, rr.Code)
			require.Equal(t, "4", rr.Header().Get("X-Expected-Offset"))
			require.Empty(t, rr.Header().Get("X-Next-Offset"))
		})
	}
	require.Equal(t, "AAAA456789abcdef", dev.content.GetString())
	requireNextOffset(t, handler, 4)
}

func TestPutBeyondDeviceSizeIsRefused(t *testing.T) {
	dev := newDevice(t, deviceContent)
	handler := newHandler(t, dev)
	require.Equal(t, http.StatusNoContent, serve(handler, newPut(0, -1, "AAAA")).Code)

	rr := serve(handler, newPut(4, -1, strings.Repeat("Z", len(deviceContent)-4+1)))

	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rr.Code)
	require.Equal(t, "AAAA456789abcdef", dev.content.GetString())
	requireNextOffset(t, handler, 4)
}

func TestPutExceedingDeclaredTotalIsRefused(t *testing.T) {
	dev := newDevice(t, deviceContent)
	handler := newHandler(t, dev)

	rr := serve(handler, newPut(0, 4, "AAAAAAAA"))

	require.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	require.Empty(t, rr.Header().Get("X-Next-Offset"))
	requireNextOffset(t, handler, 0)
}

func TestPutWithInvalidUploadHeadersIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{name: "non-numeric offset", headers: map[string]string{"X-Offset": "start"}},
		{name: "negative offset", headers: map[string]string{"X-Offset": "-4"}},
		{name: "non-numeric total", headers: map[string]string{"X-Content-Length": "all"}},
		{name: "negative total", headers: map[string]string{"X-Content-Length": "-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := newDevice(t, deviceContent)
			handler := newHandler(t, dev)
			req := newRequest(http.MethodPut, strings.NewReader("AAAA"))
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}

			rr := serve(handler, req)

			require.Equal(t, http.StatusBadRequest, rr.Code)
			require.Equal(t, deviceContent, dev.content.GetString())
			requireNextOffset(t, handler, 0)
		})
	}
}

func TestConcurrentPutIsRefused(t *testing.T) {
	dev := newDevice(t, deviceContent)
	handler := newHandler(t, dev)

	pr, pw := io.Pipe()
	first := newRequest(http.MethodPut, pr)
	first.Header.Set("X-Offset", "0")
	first.Header.Set("X-Content-Length", strconv.Itoa(len(deviceContent)))
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		done <- serve(handler, first)
	}()

	// The pipe hands the bytes over only once the first PUT reads them, so after this write
	// the first PUT is in the middle of copying its body.
	_, err := pw.Write([]byte("AAAA"))
	require.NoError(t, err)

	rr := serve(handler, newPut(0, len(deviceContent), "ZZZZ"))
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Empty(t, rr.Header().Get("X-Expected-Offset"))

	require.NoError(t, pw.Close())
	rr = <-done
	require.Equal(t, http.StatusNoContent, rr.Code, "body=%q", rr.Body.String())
	require.Equal(t, "4", rr.Header().Get("X-Next-Offset"))
	require.Equal(t, "AAAA456789abcdef", dev.content.GetString())

	rr = serve(handler, newPut(4, len(deviceContent), "BBBB"))
	require.Equal(t, http.StatusNoContent, rr.Code, "a finished PUT must release the device")
}

func TestPutWhenDeviceCannotBeOpened(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "permission denied", err: &os.PathError{Op: "open", Path: devicePath, Err: os.ErrPermission}, wantCode: http.StatusForbidden},
		{name: "other error", err: errInjected, wantCode: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := newDevice(t, deviceContent)
			f := &opFailer{}
			handler := newHandlerWith(t, failer.NewOS(dev.fsys, f), newClient(t, "", interceptor.Funcs{}))
			f.fail = func(op fs.Op, _ ...any) error {
				if op == fs.OpenOp {
					return tc.err
				}
				return nil
			}

			rr := serve(handler, newPut(0, len(deviceContent), "AAAA"))

			require.Equal(t, tc.wantCode, rr.Code)
			require.Equal(t, deviceContent, dev.content.GetString())
		})
	}
}

func TestPutWhenDeviceIOFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(op fs.Op, args ...any) error
	}{
		{name: "size lookup fails", fail: func(op fs.Op, args ...any) error {
			if op == fs.SeekOp && args[1] == io.SeekEnd {
				return errInjected
			}
			return nil
		}},
		{name: "seek to offset fails", fail: func(op fs.Op, args ...any) error {
			if op == fs.SeekOp && args[0] == int64(4) {
				return errInjected
			}
			return nil
		}},
		{name: "write fails", fail: func(op fs.Op, _ ...any) error {
			if op == fs.WriteOp {
				return errInjected
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := newDevice(t, deviceContent)
			f := &opFailer{}
			handler := newHandlerWith(t, failer.NewOS(dev.fsys, f), newClient(t, "", interceptor.Funcs{}))
			require.Equal(t, http.StatusNoContent, serve(handler, newPut(0, len(deviceContent), "AAAA")).Code)
			f.fail = tc.fail

			rr := serve(handler, newPut(4, len(deviceContent), "BBBB"))

			require.Equal(t, http.StatusInternalServerError, rr.Code)
			require.Equal(t, "AAAA456789abcdef", dev.content.GetString())
			f.fail = nil
			requireNextOffset(t, handler, 4)
		})
	}
}

func TestPostMarksImportFinished(t *testing.T) {
	repo := newClient(t, common.ServerStateReady, interceptor.Funcs{})
	handler := newHandlerWith(t, newDevice(t, deviceContent).fsys, repo)

	rr := serve(handler, newRequest(http.MethodPost, nil))

	require.Equal(t, http.StatusOK, rr.Code, "body=%q", rr.Body.String())
	di := &api.DataImport{}
	require.NoError(t, repo.Get(context.Background(), types.NamespacedName{Namespace: importNS, Name: importName}, di))
	require.Equal(t, string(common.ServerStateFinished), di.Status.ServerState)
}

func TestPostFailsWhenStateCannotBeSaved(t *testing.T) {
	repo := newClient(t, common.ServerStateReady, interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
			return errInjected
		},
	})
	handler := newHandlerWith(t, newDevice(t, deviceContent).fsys, repo)

	rr := serve(handler, newRequest(http.MethodPost, nil))

	require.Equal(t, http.StatusInternalServerError, rr.Code)
	di := &api.DataImport{}
	require.NoError(t, repo.Get(context.Background(), types.NamespacedName{Namespace: importNS, Name: importName}, di))
	require.Equal(t, string(common.ServerStateReady), di.Status.ServerState)
}

// unsizedBody hides the length of the body, as a chunked request does.
type unsizedBody struct {
	io.Reader
}

func TestPutExceedingDeclaredTotalWritesNothingPastIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(string) io.Reader
		// The device content after the refused request: a sized body is refused before anything is written,
		// an unsized one is written up to the declared total.
		want string
	}{
		{
			name: "sized body",
			body: func(s string) io.Reader { return strings.NewReader(s) },
			want: "AA23456789abcdef",
		},
		{
			name: "unsized body",
			body: func(s string) io.Reader { return unsizedBody{strings.NewReader(s)} },
			want: "AABBBB6789abcdef",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := newDevice(t, deviceContent)
			handler := newHandler(t, dev)
			require.Equal(t, http.StatusNoContent, serve(handler, newPut(0, 6, "AA")).Code)

			req := newRequest(http.MethodPut, tc.body("BBBBBBBB"))
			req.Header.Set("X-Offset", "2")
			req.Header.Set("X-Content-Length", "6")
			rr := serve(handler, req)

			require.Equal(t, http.StatusUnprocessableEntity, rr.Code)
			require.Equal(t, tc.want, dev.content.GetString())
			requireNextOffset(t, handler, 2)
		})
	}
}

func TestPutWithUnsizedBodyCompletesOnDeclaredTotal(t *testing.T) {
	dev := newDevice(t, deviceContent)
	handler := newHandler(t, dev)

	req := newRequest(http.MethodPut, unsizedBody{strings.NewReader("AAAAAA")})
	req.Header.Set("X-Offset", "0")
	req.Header.Set("X-Content-Length", "6")
	rr := serve(handler, req)

	require.Equal(t, http.StatusCreated, rr.Code, "body=%q", rr.Body.String())
	require.Equal(t, "6", rr.Header().Get("X-Next-Offset"))
	require.Equal(t, "AAAAAA6789abcdef", dev.content.GetString())
}

func TestPutFailsWhenBodyCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		body io.Reader
	}{
		{name: "before the declared total", body: io.MultiReader(strings.NewReader("AAA"), iotest.ErrReader(errInjected))},
		{name: "right after the declared total", body: io.MultiReader(strings.NewReader("AAAAAA"), iotest.ErrReader(errInjected))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := newHandler(t, newDevice(t, deviceContent))
			req := newRequest(http.MethodPut, unsizedBody{tc.body})
			req.Header.Set("X-Offset", "0")
			req.Header.Set("X-Content-Length", "6")

			rr := serve(handler, req)

			require.Equal(t, http.StatusInternalServerError, rr.Code)
			requireNextOffset(t, handler, 0)
		})
	}
}
