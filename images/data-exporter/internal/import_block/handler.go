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

package importblock

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/pkg/errors"

	"github.com/deckhouse/sds-common-lib/fs/fsext"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/repository"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/utils"
)

var (
	errNotABlockDevice    = errors.New("file is not a block device")
	errBadBlockDeviceSize = errors.New("bad block device size")
)

type ImportBlockHandler struct {
	fs           fsext.FS
	path         string
	logger       *slog.Logger
	mu           *sync.Mutex
	written      int64
	isWriting    bool
	client       *repository.Client
	cfg          *config.Config
	repository   *repository.Client
	isServiceOff bool
}

func NewImportBlockHandler(fsys fsext.FS, path string, logger *slog.Logger, client *repository.Client, cfg *config.Config, repository *repository.Client) (*ImportBlockHandler, error) {
	isServiceOff, err := client.CheckIsServiceAvailable(context.Background(), cfg.URLOpt.DataManagerNamespace, cfg.DataManagerName)
	if err != nil {
		logger.Error("failed to check if service is available", "error", err)
		return nil, err
	}

	isBlock, err := utils.IsBlockDevice(fsys, path)
	if err != nil {
		logger.Error("failed to get type of the file", "error", err)
		return nil, err
	}

	if !isBlock {
		logger.Error(fmt.Sprintf("file at path %s is not a block device", path))
		return nil, errNotABlockDevice
	}

	file, err := fsys.Open(path)
	if err != nil {
		logger.Error("failed to open block device file", "error", err)
		return nil, err
	}
	defer file.Close()

	size, err := utils.BlockDeviceSize(file)
	if err != nil {
		logger.Error("failed to get block device file size", "error", err)
		return nil, err
	}

	if size == 0 {
		logger.Error(fmt.Sprintf("block device size is 0 at path %s", path))
		return nil, errBadBlockDeviceSize
	}

	return &ImportBlockHandler{
		fs:           fsys,
		path:         path,
		logger:       logger,
		mu:           &sync.Mutex{},
		written:      0,
		isWriting:    false,
		client:       client,
		cfg:          cfg,
		repository:   client,
		isServiceOff: isServiceOff,
	}, nil
}

func (h *ImportBlockHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "" {
		http.Error(w, "volumeMode: block. not supported path. use /api/v1/block", http.StatusBadRequest)
		return
	}

	if h.isServiceOff {
		http.Error(w, "service is off", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodPut:
		h.HandlePutMethod(w, r)
	case http.MethodHead:
		h.HandleHeadMethod(w, r)
	case http.MethodPost:
		h.CompleteImport(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *ImportBlockHandler) HandleHeadMethod(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	next := h.written
	h.mu.Unlock()

	dev, err := h.fs.Open(h.path)
	if err != nil {
		h.logger.Error("failed to open block device", "error", err)
		http.Error(w, "failed to open block device", http.StatusInternalServerError)
		return
	}
	size, err := utils.BlockDeviceSize(dev)
	_ = dev.Close()
	if err != nil {
		h.logger.Error("failed to get size of the block device file", "error", err)
		http.Error(w, "failed to get block device size", http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Next-Offset", strconv.FormatInt(next, 10))
	w.Header().Set("X-Device-Size", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
}

func (h *ImportBlockHandler) HandlePutMethod(w http.ResponseWriter, r *http.Request) {
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

	offset, expectedTotal, err := utils.ParseUploadHeaders(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	dev, err := os.OpenFile(h.path, os.O_WRONLY, 0)
	if err != nil {
		if os.IsPermission(err) {
			http.Error(w, "permission denied to write block device", http.StatusForbidden)
			return
		}
		h.logger.Error("failed to open block device", "error", err)
		http.Error(w, "failed to open block device", http.StatusInternalServerError)
		return
	}
	defer dev.Close()

	size, err := utils.BlockDeviceSize(dev)
	if err != nil {
		h.logger.Error("failed to get size of the block device file", "error", err)
		http.Error(w, "failed to get block device size", http.StatusInternalServerError)
		return
	}
	if size <= 0 {
		http.Error(w, "bad block device size", http.StatusInternalServerError)
		return
	}

	h.mu.Lock()
	current := h.written
	h.mu.Unlock()

	if offset != current {
		w.Header().Set("X-Expected-Offset", strconv.FormatInt(current, 10))
		http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
		return
	}
	if offset < 0 || offset > size {
		http.Error(w, http.StatusText(http.StatusRequestedRangeNotSatisfiable), http.StatusRequestedRangeNotSatisfiable)
		return
	}

	remain := size - offset
	if r.ContentLength >= 0 && r.ContentLength > remain {
		http.Error(w, http.StatusText(http.StatusRequestedRangeNotSatisfiable), http.StatusRequestedRangeNotSatisfiable)
		return
	}

	if _, err := dev.Seek(offset, io.SeekStart); err != nil {
		http.Error(w, "failed to seek", http.StatusInternalServerError)
		return
	}

	written, werr := io.Copy(dev, r.Body)
	if werr != nil && !errors.Is(werr, io.EOF) {
		h.logger.Error("failed to write data into block device", "error", werr)
		http.Error(w, "failed to write data", http.StatusInternalServerError)
		return
	}

	newOffset := offset + written
	if expectedTotal >= 0 && newOffset > expectedTotal {
		http.Error(w, "written exceeds expected total size", http.StatusUnprocessableEntity)
		return
	}

	h.mu.Lock()
	h.written = newOffset
	h.mu.Unlock()

	w.Header().Set("X-Next-Offset", strconv.FormatInt(newOffset, 10))
	if expectedTotal >= 0 && newOffset == expectedTotal {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ImportBlockHandler) CompleteImport(w http.ResponseWriter, _ *http.Request) {
	err := h.client.SetDataImportCompleted(context.Background(), h.cfg.URLOpt.DataManagerNamespace, h.cfg.DataManagerName)
	if err != nil {
		h.logger.Error("failed to set data import completed", "error", err)
		http.Error(w, "failed to set data import completed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
