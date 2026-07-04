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

package export_block

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/deckhouse/sds-common-lib/fs/fsext"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/utils"
)

type BlockExporter struct {
	fs     fsext.FS
	path   string
	logger *slog.Logger
}

func NewBlockExporter(fsys fsext.FS, path string, logger *slog.Logger) (*BlockExporter, error) {
	isBlock, err := utils.IsBlockDevice(fsys, path)
	if err != nil {
		logger.Error("Failed to get type of the file", "error", err)
		return nil, err
	}

	if !isBlock {
		err := errors.New("file is not a block device")
		logger.Error(err.Error())
		return nil, err
	}

	file, err := fsys.Open(path)
	if err != nil {
		logger.Error("Failed to open block device file", "error", err)
		return nil, err
	}
	defer file.Close()

	size, err := utils.BlockDeviceSize(file)
	if err != nil {
		logger.Error("Failed to get block device file size", "error", err)
		return nil, err
	}

	if size == 0 {
		err := errors.New("bad block device size")
		logger.Error(err.Error())
		return nil, err
	}

	return &BlockExporter{
		fsys,
		path,
		logger,
	}, nil
}

func (e *BlockExporter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var err error

	switch r.Method {
	case http.MethodGet:
		err = e.HandleGetMethod(w, r)
	case http.MethodHead:
		err = e.HandleHeadMethod(w, r)
	default:
		err = &utils.HTTPError{Err: errors.New("supports only methods GET and HEAD"), Status: http.StatusMethodNotAllowed}
	}

	var httpError *utils.HTTPError
	if errors.As(err, &httpError) {
		http.Error(w, httpError.Error(), httpError.Status)
	} else if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (e *BlockExporter) HandleHeadMethod(w http.ResponseWriter, r *http.Request) error {
	return e.prepareHead(w, r)
}

func (e *BlockExporter) HandleGetMethod(w http.ResponseWriter, r *http.Request) error {
	err := e.prepareHead(w, r)
	if err != nil {
		return err
	}

	file, err := e.fs.Open(e.path)
	if err != nil {
		e.logger.Error("failed to open block device file", "error", err)
		return err
	}

	fi, err := file.Stat()
	if err != nil {
		e.logger.Error("Failed to get stat of the block device file", "error", err)
		return err
	}

	defer file.Close()

	http.ServeContent(w, r, fi.Name(), fi.ModTime(), file)

	return nil
}

func (e *BlockExporter) prepareHead(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Path != "" {
		e.logger.Error("unexpected path", "requestUrlPath", r.URL.Path, "expected", "/")
		return &utils.HTTPError{
			Err:    errors.New("volumeMode: block. Not supported path. Use /api/v1/block to download raw block"),
			Status: http.StatusBadRequest,
		}
	}

	file, err := e.fs.Open(e.path)
	if err != nil {
		e.logger.Error("failed to open block device file", "error", err)
		return err
	}
	defer file.Close()

	// Use fixed filename to prevent block device mount point disclosure
	w.Header().Set("Content-Disposition", "attachment; filename=data.img")
	w.Header().Set("Content-Type", "application/octet-stream")

	size, err := utils.BlockDeviceSize(file)
	if err != nil {
		e.logger.Error("Failed to get size of the block device file", "error", err)
		return err
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))

	return err
}

func WrongPrefixFilesInformer(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "VolumeMode: Block. Not supported downloading files. Use /api/v1/block", http.StatusBadRequest)
}

func WrongPrefixBlockInformer(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "" {
		http.Error(w, "VolumeMode: Block. Not supported for listing files. Use /api/v1/block", http.StatusBadRequest)
		return
	}
	http.Error(w, "VolumeMode: Block. Not supported path. Use /api/v1/block to download raw block", http.StatusBadRequest)
}
