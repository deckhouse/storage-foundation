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

package utils

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"time"
)

var (
	errInvalidPermissionsOctalNumber = errors.New("invalid X-Attribute-Permissions header: must be a valid octal number")
	errInvalidPermissionsValue       = errors.New("invalid X-Attribute-Permissions header: value must be between 0 and 0777")
	errInvalidUID                    = errors.New("invalid X-Attribute-Uid header: must be a valid integer")
	errInvalidGid                    = errors.New("invalid X-Attribute-Gid header: must be a valid integer")
	errInvalidModTime                = errors.New("invalid X-Attribute-ModTime header")
	errInvalidOffset                 = errors.New("invalid X-Offset header: must be a non-negative integer")
	errInvalidContentLength          = errors.New("invalid X-Content-Length header: must be a non-negative integer")
)

func ParseFileAttributes(r *http.Request) (perm *uint32, uid, gid int, err error) {
	uid, gid = -1, -1

	if modeStr := r.Header.Get("X-Attribute-Permissions"); modeStr != "" {
		mode, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return nil, -1, -1, errInvalidPermissionsOctalNumber
		}
		if mode > 0o777 {
			return nil, -1, -1, errInvalidPermissionsValue
		}
		mode32 := uint32(mode)
		perm = &mode32
	}

	if uidStr := r.Header.Get("X-Attribute-Uid"); uidStr != "" {
		uid64, err := strconv.ParseInt(uidStr, 10, 32)
		if err != nil {
			return nil, -1, -1, errInvalidUID
		}
		if uid64 < 0 {
			return nil, -1, -1, errInvalidUID
		}
		uid = int(uid64)
	}

	if gidStr := r.Header.Get("X-Attribute-Gid"); gidStr != "" {
		gid64, err := strconv.ParseInt(gidStr, 10, 32)
		if err != nil {
			return nil, -1, -1, errInvalidGid
		}
		if gid64 < 0 {
			return nil, -1, -1, errInvalidGid
		}
		gid = int(gid64)
	}

	return perm, uid, gid, nil
}

func SetFileAttributes(absPath string, perm fs.FileMode, uid, gid int, mtime time.Time) error {
	if perm != 0 {
		if err := os.Chmod(absPath, perm); err != nil {
			return errInvalidPermissionsOctalNumber
		}
	}
	if uid >= 0 && gid >= 0 {
		if err := os.Chown(absPath, uid, gid); err != nil {
			return errInvalidUID
		}
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(absPath, time.Time{}, mtime); err != nil {
			return errInvalidModTime
		}
	}
	return nil
}

func ParseExtraAttributes(r *http.Request) (modTime time.Time, linkTarget string, err error) {
	if modTimeStr := r.Header.Get("X-Attribute-ModTime"); modTimeStr != "" {
		if modTime, err = time.Parse(time.RFC3339, modTimeStr); err != nil {
			return time.Time{}, "", errInvalidModTime
		}
	}

	linkTarget = r.Header.Get("X-LinkTarget")

	return
}

func ParseUploadHeaders(r *http.Request) (offset int64, expectedTotal int64, err error) {
	offset = 0
	expectedTotal = -1

	if offStr := r.Header.Get("X-Offset"); offStr != "" {
		val, perr := strconv.ParseInt(offStr, 10, 64)
		if perr != nil || val < 0 {
			return 0, -1, errInvalidOffset
		}
		offset = val
	}

	if totalStr := r.Header.Get("X-Content-Length"); totalStr != "" {
		val, perr := strconv.ParseInt(totalStr, 10, 64)
		if perr != nil || val < 0 {
			return 0, -1, errInvalidContentLength
		}
		expectedTotal = val
	}

	return offset, expectedTotal, nil
}
