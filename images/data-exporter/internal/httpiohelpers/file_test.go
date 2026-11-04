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

package httpiohelpers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newRequestWithHeaders(headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/", http.NoBody)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return req
}

func permOf(mode uint32) *uint32 {
	return &mode
}

func TestParseFileAttributes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		headers  map[string]string
		wantPerm *uint32
		wantUID  int
		wantGID  int
		wantErr  error
	}{
		{name: "no headers", wantUID: -1, wantGID: -1},
		{name: "permissions with leading zero", headers: map[string]string{"X-Attribute-Permissions": "0755"}, wantPerm: permOf(0o755), wantUID: -1, wantGID: -1},
		{name: "permissions without leading zero", headers: map[string]string{"X-Attribute-Permissions": "640"}, wantPerm: permOf(0o640), wantUID: -1, wantGID: -1},
		{name: "zero permissions", headers: map[string]string{"X-Attribute-Permissions": "0"}, wantPerm: permOf(0), wantUID: -1, wantGID: -1},
		{name: "highest permissions", headers: map[string]string{"X-Attribute-Permissions": "0777"}, wantPerm: permOf(0o777), wantUID: -1, wantGID: -1},
		{name: "permissions with special bits", headers: map[string]string{"X-Attribute-Permissions": "1777"}, wantErr: errInvalidPermissionsValue},
		{name: "permissions with go octal prefix", headers: map[string]string{"X-Attribute-Permissions": "0o755"}, wantErr: errInvalidPermissionsOctalNumber},
		{name: "permissions with non-octal digit", headers: map[string]string{"X-Attribute-Permissions": "0758"}, wantErr: errInvalidPermissionsOctalNumber},
		{name: "negative permissions", headers: map[string]string{"X-Attribute-Permissions": "-1"}, wantErr: errInvalidPermissionsOctalNumber},
		{name: "uid and gid", headers: map[string]string{"X-Attribute-Uid": "1000", "X-Attribute-Gid": "2000"}, wantUID: 1000, wantGID: 2000},
		{name: "uid only", headers: map[string]string{"X-Attribute-Uid": "0"}, wantUID: 0, wantGID: -1},
		{name: "gid only", headers: map[string]string{"X-Attribute-Gid": "0"}, wantUID: -1, wantGID: 0},
		{name: "all attributes", headers: map[string]string{"X-Attribute-Permissions": "0600", "X-Attribute-Uid": "1", "X-Attribute-Gid": "2"}, wantPerm: permOf(0o600), wantUID: 1, wantGID: 2},
		{name: "negative uid", headers: map[string]string{"X-Attribute-Uid": "-1"}, wantErr: errInvalidUID},
		{name: "non-numeric uid", headers: map[string]string{"X-Attribute-Uid": "root"}, wantErr: errInvalidUID},
		{name: "uid out of int32 range", headers: map[string]string{"X-Attribute-Uid": strconv.FormatInt(1<<31, 10)}, wantErr: errInvalidUID},
		{name: "negative gid", headers: map[string]string{"X-Attribute-Gid": "-1"}, wantErr: errInvalidGid},
		{name: "non-numeric gid", headers: map[string]string{"X-Attribute-Gid": "wheel"}, wantErr: errInvalidGid},
		{name: "gid out of int32 range", headers: map[string]string{"X-Attribute-Gid": strconv.FormatInt(1<<31, 10)}, wantErr: errInvalidGid},
		{name: "valid permissions and invalid uid", headers: map[string]string{"X-Attribute-Permissions": "0600", "X-Attribute-Uid": "x"}, wantErr: errInvalidUID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			perm, uid, gid, err := ParseFileAttributes(newRequestWithHeaders(tc.headers))

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Nil(t, perm)
				require.Equal(t, -1, uid)
				require.Equal(t, -1, gid)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantPerm, perm)
			require.Equal(t, tc.wantUID, uid)
			require.Equal(t, tc.wantGID, gid)
		})
	}
}

func TestParseExtraAttributes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		headers     map[string]string
		wantModTime time.Time
		wantLink    string
		wantErr     error
	}{
		{name: "no headers"},
		{name: "utc mod time", headers: map[string]string{"X-Attribute-ModTime": "2026-01-02T03:04:05Z"}, wantModTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "mod time with zone offset", headers: map[string]string{"X-Attribute-ModTime": "2026-01-02T06:04:05+03:00"}, wantModTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "mod time with fractional seconds", headers: map[string]string{"X-Attribute-ModTime": "2026-01-02T03:04:05.5Z"}, wantModTime: time.Date(2026, 1, 2, 3, 4, 5, 500000000, time.UTC)},
		{name: "link target", headers: map[string]string{"X-LinkTarget": "../target"}, wantLink: "../target"},
		{name: "mod time and link target", headers: map[string]string{"X-Attribute-ModTime": "2026-01-02T03:04:05Z", "X-LinkTarget": "target"}, wantModTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), wantLink: "target"},
		{name: "mod time as unix seconds", headers: map[string]string{"X-Attribute-ModTime": "1767323045"}, wantErr: errInvalidModTime},
		{name: "mod time without zone", headers: map[string]string{"X-Attribute-ModTime": "2026-01-02T03:04:05"}, wantErr: errInvalidModTime},
		{name: "invalid mod time with link target", headers: map[string]string{"X-Attribute-ModTime": "yesterday", "X-LinkTarget": "target"}, wantErr: errInvalidModTime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modTime, link, err := ParseExtraAttributes(newRequestWithHeaders(tc.headers))

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.True(t, modTime.IsZero())
				require.Empty(t, link)
				return
			}
			require.NoError(t, err)
			require.True(t, tc.wantModTime.Equal(modTime), "want %v, got %v", tc.wantModTime, modTime)
			require.Equal(t, tc.wantLink, link)
		})
	}
}

func TestParseUploadHeaders(t *testing.T) {
	for _, tc := range []struct {
		name       string
		headers    map[string]string
		wantOffset int64
		wantTotal  int64
		wantErr    error
	}{
		{name: "no headers", wantOffset: 0, wantTotal: -1},
		{name: "offset only", headers: map[string]string{"X-Offset": "4096"}, wantOffset: 4096, wantTotal: -1},
		{name: "total only", headers: map[string]string{"X-Content-Length": "8192"}, wantOffset: 0, wantTotal: 8192},
		{name: "offset and total", headers: map[string]string{"X-Offset": "10", "X-Content-Length": "20"}, wantOffset: 10, wantTotal: 20},
		{name: "zero total", headers: map[string]string{"X-Content-Length": "0"}, wantOffset: 0, wantTotal: 0},
		{name: "offset beyond total is not checked here", headers: map[string]string{"X-Offset": "30", "X-Content-Length": "20"}, wantOffset: 30, wantTotal: 20},
		{name: "largest offset", headers: map[string]string{"X-Offset": strconv.FormatInt(1<<63-1, 10)}, wantOffset: 1<<63 - 1, wantTotal: -1},
		{name: "negative offset", headers: map[string]string{"X-Offset": "-1"}, wantErr: errInvalidOffset},
		{name: "non-numeric offset", headers: map[string]string{"X-Offset": "start"}, wantErr: errInvalidOffset},
		{name: "offset out of int64 range", headers: map[string]string{"X-Offset": "9223372036854775808"}, wantErr: errInvalidOffset},
		{name: "negative total", headers: map[string]string{"X-Content-Length": "-1"}, wantErr: errInvalidContentLength},
		{name: "non-numeric total", headers: map[string]string{"X-Content-Length": "1k"}, wantErr: errInvalidContentLength},
		{name: "valid offset and invalid total", headers: map[string]string{"X-Offset": "1", "X-Content-Length": "x"}, wantErr: errInvalidContentLength},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offset, total, err := ParseUploadHeaders(newRequestWithHeaders(tc.headers))

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Zero(t, offset)
				require.Equal(t, int64(-1), total)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantOffset, offset)
			require.Equal(t, tc.wantTotal, total)
		})
	}
}

func TestSetFileAttributes(t *testing.T) {
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	newFile := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(path, []byte("content"), 0o600))
		require.NoError(t, os.Chmod(path, 0o600))
		return path
	}

	t.Run("applies permissions, owner and mtime", func(t *testing.T) {
		path := newFile(t)

		require.NoError(t, SetFileAttributes(path, 0o640, os.Getuid(), os.Getgid(), mtime))

		fi, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o640), fi.Mode().Perm())
		require.True(t, mtime.Equal(fi.ModTime()), "want %v, got %v", mtime, fi.ModTime())
	})

	t.Run("zero values leave the file as is", func(t *testing.T) {
		path := newFile(t)
		before, err := os.Stat(path)
		require.NoError(t, err)

		require.NoError(t, SetFileAttributes(path, 0, -1, -1, time.Time{}))

		after, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, before.Mode(), after.Mode())
		require.Equal(t, before.ModTime(), after.ModTime())
	})

	t.Run("owner needs both uid and gid", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")

		require.NoError(t, SetFileAttributes(missing, 0, os.Getuid(), -1, time.Time{}))
		require.NoError(t, SetFileAttributes(missing, 0, -1, os.Getgid(), time.Time{}))
	})

	missing := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name    string
		perm    os.FileMode
		uid     int
		gid     int
		mtime   time.Time
		wantErr error
	}{
		{name: "chmod fails", perm: 0o640, uid: -1, gid: -1, wantErr: errInvalidPermissionsOctalNumber},
		{name: "chown fails", uid: os.Getuid(), gid: os.Getgid(), wantErr: errInvalidUID},
		{name: "chtimes fails", uid: -1, gid: -1, mtime: mtime, wantErr: errInvalidModTime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, SetFileAttributes(missing, tc.perm, tc.uid, tc.gid, tc.mtime), tc.wantErr)
		})
	}
}

func TestHTTPErrorKeepsMessageOfWrappedError(t *testing.T) {
	err := &HTTPError{Err: errors.New("file not found"), Status: http.StatusNotFound}

	require.EqualError(t, err, "file not found")
}
