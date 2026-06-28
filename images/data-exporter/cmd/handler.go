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
	"fmt"
	"log/slog"
	"net/http"

	"github.com/deckhouse/sds-common-lib/fs/fsext"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/export_block"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/export_filesystem"
	importblock "github.com/deckhouse/storage-foundation/images/data-exporter/internal/import_block"
	importfilesystem "github.com/deckhouse/storage-foundation/images/data-exporter/internal/import_filesystem"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/repository"
)

func NewExporterHandler(fsys fsext.FS, path string, mode config.VolumeMode, logger *slog.Logger) (http.Handler, error) {
	switch mode {
	case config.VolumeModeBlock:
		return export_block.NewBlockExporter(fsys, path, logger)
	case config.VolumeModeFilesystem:
		return export_filesystem.NewFilesystemExporter(fsys, path, logger)
	default:
		return nil, fmt.Errorf("invalid program argument, mode %v", mode)
	}
}

func NewImportHandler(fsys fsext.FS, path string, mode config.VolumeMode, logger *slog.Logger, client *repository.Client, cfg *config.Config, repository *repository.Client) (http.Handler, error) {
	switch mode {
	case config.VolumeModeBlock:
		return importblock.NewImportBlockHandler(fsys, path, logger, client, cfg, repository)
	case config.VolumeModeFilesystem:
		return importfilesystem.NewImportFilesystemHandler(fsys, path, logger, client, cfg, repository)
	default:
		return nil, fmt.Errorf("invalid program argument, mode %v", mode)
	}
}

// Add handlers for filesystem export mode
func MuxAddFSHandler(mux *http.ServeMux, opt config.URLOpt, handler http.Handler, logger *slog.Logger) {
	muxAddHandler(mux, "files/", opt, handler, logger)
	muxAddHandler(mux, "files", opt, http.HandlerFunc(export_filesystem.WrongPrefixFilesInformer), logger)
	muxAddHandler(mux, "block", opt, http.HandlerFunc(export_filesystem.WrongPrefixBlockInformer), logger)
	muxAddHandler(mux, "block/", opt, http.HandlerFunc(export_filesystem.WrongPrefixBlockInformer), logger)
}

// Add handlers for block device export mode
func MuxAddBlockHandler(mux *http.ServeMux, opt config.URLOpt, handler http.Handler, logger *slog.Logger) {
	muxAddHandler(mux, "block", opt, handler, logger)
	muxAddHandler(mux, "block/", opt, http.HandlerFunc(export_block.WrongPrefixBlockInformer), logger)
	muxAddHandler(mux, "files", opt, http.HandlerFunc(export_block.WrongPrefixFilesInformer), logger)
	muxAddHandler(mux, "files/", opt, http.HandlerFunc(export_block.WrongPrefixFilesInformer), logger)
}

func muxAddHandler(mux *http.ServeMux, suffix string, opt config.URLOpt, handler http.Handler, logger *slog.Logger) {
	shortPrefix := fmt.Sprintf("/api/v1/%s", suffix)
	longPrefix := fmt.Sprintf("%sapi/v1/%s", GetPathPrefix(opt), suffix)
	if logger != nil {
		logger.Info(
			"Registering HTTP handler",
			"shortPathPrefix", shortPrefix,
			"servedPathPrefix", GetPathPrefix(opt),
			"longPathPrefix", longPrefix,
		)
	}
	mux.Handle(longPrefix, http.StripPrefix(longPrefix, handler))
	mux.Handle(shortPrefix, http.StripPrefix(shortPrefix, handler))
}

func GetPathPrefix(opt config.URLOpt) string {
	return fmt.Sprintf("/%s/%s/%s/", opt.DataManagerNamespace, opt.DataManagerTargetKindShort, opt.DataManagerTargetName)
}

// Register unified POST /api/v1/finished endpoint (short and long prefixes)
func MuxAddFinishedHandler(mux *http.ServeMux, opt config.URLOpt, client *repository.Client, cfg *config.Config, logger *slog.Logger) {
	finishedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := client.SetDataImportCompleted(r.Context(), cfg.URLOpt.DataManagerNamespace, cfg.DataManagerName); err != nil {
			logger.Error("failed to set data import completed", "error", err)
			http.Error(w, "failed to set data import completed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	shortFinished := "/api/v1/finished"
	longFinished := GetPathPrefix(opt) + shortFinished
	mux.Handle(longFinished, finishedHandler)
	mux.Handle(shortFinished, finishedHandler)
}
