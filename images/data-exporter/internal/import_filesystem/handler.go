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

package importfilesystem

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/deckhouse/sds-common-lib/fs/fsext"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/httpiohelpers"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/repository"
)

var (
	errPathMustBeAbsolute = errors.New("path must be absolute")
)

const (
	defaultDirPerm  = os.FileMode(0o750)
	defaultFilePerm = os.FileMode(0o640)
)

type ImportFilesystemHandler struct {
	fs           fsext.FS
	path         string
	logger       *slog.Logger
	mu           *sync.Mutex
	isWriting    bool
	client       *repository.Client
	cfg          *config.Config
	repository   *repository.Client
	isServiceOff bool
}

func NewImportFilesystemHandler(fsys fsext.FS, path string, logger *slog.Logger, client *repository.Client, cfg *config.Config, repository *repository.Client) (*ImportFilesystemHandler, error) {
	isServiceOff, err := client.CheckIsServiceAvailable(context.Background(), cfg.URLOpt.DataManagerNamespace, cfg.DataManagerName)
	if err != nil {
		logger.Error("failed to check if service is available", "error", err)
		return nil, err
	}

	cleanPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanPath) {
		logger.Error("path must be absolute", "path", path)
		return nil, errPathMustBeAbsolute
	}

	if err := checkIsDir(fsys, cleanPath); err != nil {
		logger.Error("Failed to initialize filesystem importer", "path", cleanPath, "error", err)
		return nil, err
	}

	if _, err := fsys.ReadDir(cleanPath); err != nil {
		logger.Error("Failed to read directory", "path", cleanPath, "error", err)
		return nil, err
	}

	return &ImportFilesystemHandler{fsys, cleanPath, logger, &sync.Mutex{}, false, client, cfg, repository, isServiceOff}, nil
}

func (h *ImportFilesystemHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.isServiceOff {
		http.Error(w, "service is off", http.StatusForbidden)
		return
	}

	isDir := r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodPut:
		h.CreateFile(w, r)
	case http.MethodPost:
		h.CompleteImport(w, r)
	case http.MethodHead:
		if isDir {
			http.Error(w, "HEAD is supported for files only", http.StatusBadRequest)
			return
		}
		h.HeadFile(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *ImportFilesystemHandler) CreateFile(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.isWriting {
		h.mu.Unlock()
		http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
		return
	}
	h.isWriting = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.isWriting = false
		h.mu.Unlock()
	}()

	perm, uid, gid, err := httpiohelpers.ParseFileAttributes(r)
	if err != nil {
		h.logger.Error("failed to parse file attributes", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	modTime, _, err := httpiohelpers.ParseExtraAttributes(r)
	if err != nil {
		h.logger.Error("failed to parse extra attributes", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, expectedTotal, err := httpiohelpers.ParseUploadHeaders(r)
	if err != nil {
		h.logger.Error("failed to parse upload headers", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	urlPath := filepath.Clean(r.URL.Path)
	absPath := filepath.Join(h.path, urlPath)
	tempPath := absPath + ".tempupload"
	parentDir := filepath.Dir(absPath)

	if err := h.fs.MkdirAll(parentDir, defaultDirPerm); err != nil {
		if !os.IsExist(err) {
			h.logger.Error("failed to create parent directories", "dir", parentDir, "error", err)
			http.Error(w, "failed to create parent directories", http.StatusInternalServerError)
			return
		}
	}

	// Check if target file exists
	var exists bool
	var currentSize int64
	if fi, err := h.fs.Lstat(absPath); err == nil {
		exists = true
		currentSize = fi.Size()
	} else if os.IsNotExist(err) {
		exists = false
		currentSize = 0
	} else {
		h.logger.Error("failed to stat file path", "path", absPath, "error", err)
		http.Error(w, "failed to check file", http.StatusInternalServerError)
		return
	}

	// Check if file is fully uploaded
	isUploaded := exists && expectedTotal >= 0 && currentSize == expectedTotal
	if isUploaded {
		h.logger.Info("file already exists", "path", absPath, "size", currentSize)
		http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
		return
	}

	filePerm := defaultFilePerm
	if perm != nil {
		filePerm = os.FileMode(*perm)
	}

	// Check if temporary file exists
	var tempExists bool
	if fi, err := h.fs.Lstat(tempPath); err == nil {
		tempExists = true
		currentSize = fi.Size()
	} else if os.IsNotExist(err) {
		tempExists = false
	} else {
		h.logger.Error("failed to stat temporary file path", "path", tempPath, "error", err)
		http.Error(w, "failed to check temporary file", http.StatusInternalServerError)
		return
	}

	var f *os.File
	if !tempExists {
		if offset != 0 {
			h.logger.Info("upload offset mismatch", "path", absPath, "offset", offset)
			w.Header().Set("X-Expected-Offset", "0")
			http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
			return
		}
		f, err = os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	} else {
		if offset != currentSize {
			h.logger.Info("upload offset mismatch", "path", absPath, "currentSize", currentSize, "offset", offset)
			w.Header().Set("X-Expected-Offset", strconv.FormatInt(currentSize, 10))
			http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
			return
		}

		f, err = os.OpenFile(tempPath, os.O_WRONLY, 0)
		if err == nil {
			if _, err = f.Seek(offset, io.SeekStart); err != nil {
				h.logger.Error("failed to seek in file", "path", tempPath, "offset", offset, "error", err)
				_ = f.Close()
				http.Error(w, "failed to seek", http.StatusInternalServerError)
				return
			}
		}
	}
	if err != nil {
		h.logger.Error("failed to open file", "path", tempPath, "error", err)
		http.Error(w, "Error file access denied", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	written, copyErr := io.Copy(f, r.Body)
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		h.logger.Error("failed to write request body to file", "path", tempPath, "error", copyErr)
		http.Error(w, "Error writing request body", http.StatusInternalServerError)
		return
	}

	err = f.Close()
	if err != nil {
		h.logger.Error("failed to close file after write", "path", tempPath, "error", err)
		http.Error(w, "Error closing file", http.StatusInternalServerError)
		return
	}

	newSize := currentSize + written
	if expectedTotal >= 0 && newSize == expectedTotal {
		w.WriteHeader(http.StatusCreated)

		if err := os.Rename(tempPath, absPath); err != nil {
			h.logger.Error("failed to rename temporary file", "from", tempPath, "to", absPath, "error", err)
			http.Error(w, "error renaming temporary file", http.StatusInternalServerError)
			return
		}
	} else {
		w.Header().Set("X-Next-Offset", strconv.FormatInt(newSize, 10))
		w.WriteHeader(http.StatusNoContent)
	}

	if err := httpiohelpers.SetFileAttributes(absPath, filePerm, uid, gid, modTime); err != nil {
		h.logger.Error("failed to set file attributes", "path", absPath, "error", err)
		http.Error(w, "error setting file attributes", http.StatusInternalServerError)
		return
	}
}

func (h *ImportFilesystemHandler) CompleteImport(w http.ResponseWriter, _ *http.Request) {
	err := h.client.SetDataImportCompleted(context.Background(), h.cfg.URLOpt.DataManagerNamespace, h.cfg.DataManagerName)
	if err != nil {
		h.logger.Error("failed to set data import completed", "error", err)
		http.Error(w, "failed to set data import completed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *ImportFilesystemHandler) HeadFile(w http.ResponseWriter, r *http.Request) {
	urlPath := filepath.Clean(r.URL.Path)
	absPath := filepath.Join(h.path, urlPath)
	tempPath := absPath + ".tempupload"

	tfi, err := h.fs.Lstat(tempPath)
	if err == nil {
		w.Header().Set("X-Next-Offset", strconv.FormatInt(tfi.Size(), 10))
		w.WriteHeader(http.StatusOK)
		return
	} else if !os.IsNotExist(err) {
		h.logger.Error("failed to stat temporary file path", "path", tempPath, "error", err)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	fi, err := h.fs.Lstat(absPath)
	if err != nil {
		w.Header().Set("Content-Length", "0")
		if os.IsNotExist(err) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.logger.Error("failed to stat file path", "path", absPath, "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
}

func checkIsDir(fsys fsext.FS, path string) error {
	fi, err := fsys.Lstat(path)
	if err != nil {
		return err
	}

	if !fi.IsDir() {
		return errors.New("not a directory")
	}

	return nil
}
