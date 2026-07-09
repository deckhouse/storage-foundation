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

package export_filesystem

import (
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/deckhouse/sds-common-lib/fs/fsext"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/httpiohelpers"
)

const dirListChunkSize = 10

const (
	file      ItemType = "file"
	directory ItemType = "dir"
	link      ItemType = "link"
	linkErr   ItemType = "linkErr"
	other     ItemType = "other"
)

type ItemType string

type Response struct {
	APIVersion string `json:"apiVersion"`
	Items      []Item `json:"items"`
}

type Item struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	URI        string         `json:"uri"`
	TargetPath string         `json:"targetPath,omitempty"`
	Attributes map[string]any `json:"attributes"`
}

var mapAttributesToHeader = map[string]string{
	"size":        "Content-Length",
	"modtime":     "X-Attribute-Modtime",
	"gid":         "X-Attribute-Gid",
	"uid":         "X-Attribute-Uid",
	"permissions": "X-Attribute-Permissions",
	"hash.md5":    "X-Attribute-Hash-Md5",
}

// requested attributesMask mask
type attributesMask struct {
	stat    bool
	hashMd5 bool
}

type FilesystemExporter struct {
	Root   string
	fs     fsext.FS
	logger *slog.Logger
}

func NewFilesystemExporter(fsys fsext.FS, root string, logger *slog.Logger) (*FilesystemExporter, error) {
	err := checkIsDir(fsys, root)
	if err != nil {
		logger.Error("Failed to initialize file system exporter", "error", err)
		return nil, err
	}

	return &FilesystemExporter{root, fsys, logger}, nil
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

func (d *FilesystemExporter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var requestedType ItemType
	if strings.HasSuffix(r.URL.Path, "/") || (r.URL.Path == "") {
		requestedType = directory
	} else {
		requestedType = file
	}

	var err error

	switch r.Method {
	case http.MethodGet:
		err = d.HandleGetMethod(w, r, requestedType)
	case http.MethodHead:
		err = d.HandleHeadMethod(w, r, requestedType)
	default:
		err = &httpiohelpers.HTTPError{Err: errors.New("supports only methods GET and HEAD"), Status: http.StatusMethodNotAllowed}
	}

	var httpError *httpiohelpers.HTTPError
	if errors.As(err, &httpError) {
		http.Error(w, httpError.Error(), httpError.Status)
	} else if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func getRequestedAttributes(url *url.URL) attributesMask {
	var mask attributesMask
	params := url.Query()

	values, ok := params["attribute"]
	if !ok {
		return mask
	}

	mask.stat = slices.Contains(values, "stat")
	mask.hashMd5 = slices.Contains(values, "hash.md5")

	return mask
}

// Validates that path doesn't escape the root directory and doesn't have symlinks
// Returns absolute path and file info
func (d *FilesystemExporter) resolvePath(urlPath string) (string, fs.FileInfo, error) {
	// Notes
	// - Prevent ".." escaping from the root
	// - We don't follow symlinks neither at  the end of the path nor in the middle of the path
	//   because filesystem exporter doesn't need it
	// - We don't care directory renaming attack because user has no access to the
	//   storage-foundation container

	// URL path is always an absolute path
	// Clean() prevents path to escape the root, e.g. "/.." -> "/"
	// Therefore, we don't have to handle ".."
	path := filepath.Clean(urlPath)
	segments := strings.Split(path, "/") // NOTE: safely use "/" because path is from URL

	// Inspect path segment by segment to check no symlinks occure
	prefix := d.Root
	var fi fs.FileInfo
	for i, segment := range segments {
		var err error

		prefix = filepath.Join(prefix, segment)
		fi, err = d.fs.Lstat(prefix)

		if err != nil {
			return "", nil, err
		}

		isLastSegment := i == len(segments)-1
		if !isLastSegment && isSymlink(fi.Mode()) {
			return "", nil, &httpiohelpers.HTTPError{Err: fmt.Errorf("symlink is not allowed, symlink=%s, path=%s", prefix, urlPath), Status: http.StatusBadRequest}
		}
	}

	// We've already get file info, so let caller reuse it
	return filepath.Join(d.Root, path), fi, nil
}

// Common request processing for HEAD and GET
func (d *FilesystemExporter) prepareHead(w http.ResponseWriter, r *http.Request, requestedType ItemType) (string, error) {
	var pathErr *fs.PathError
	var httpErr *httpiohelpers.HTTPError

	path, fi, err := d.resolvePath(r.URL.Path)

	switch {
	case errors.As(err, &pathErr):
		return "", &httpiohelpers.HTTPError{Err: fmt.Errorf("file not found %w", err), Status: http.StatusNotFound}
	case errors.As(err, &httpErr):
		return "", err
	case err != nil:
		return "", err
	}

	if err := d.validateFileType(fi, requestedType); err != nil {
		return "", err
	}

	if err := d.writeRequiredHeaders(w, path, fi); err != nil {
		return "", err
	}

	if err := d.writeAttributes(w, r, path, fi); err != nil {
		return "", err
	}

	return path, nil
}

func (d *FilesystemExporter) validateFileType(fi fs.FileInfo, requestedType ItemType) error {
	isDir := fi.IsDir()
	isFile := fi.Mode().IsRegular()
	isLink := isSymlink(fi.Mode())

	if isDir && requestedType == file {
		return &httpiohelpers.HTTPError{Err: errors.New("file is requested, but it's a directory"), Status: http.StatusBadRequest}
	} else if (isFile || isLink) && requestedType == directory {
		return &httpiohelpers.HTTPError{Err: errors.New("directory is requested, but it's a file"), Status: http.StatusBadRequest}
	}

	if !isDir && !isFile && !isLink {
		return &httpiohelpers.HTTPError{Err: errors.New("requested file is not a regular file, directory or symlink"), Status: http.StatusBadRequest}
	}

	return nil
}

func (d *FilesystemExporter) writeRequiredHeaders(w http.ResponseWriter, path string, fi fs.FileInfo) error {
	w.Header().Set("X-Type", string(modeToType(fi.Mode())))

	switch {
	case fi.Mode().IsDir():
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Transfer-Encoding", "chunked")
	case fi.Mode().IsRegular():
		// File-specific headers
		fname := filepath.Base(path)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(fi.Size()))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fname))
	case isSymlink(fi.Mode()):
		// Symlink-specific headers
		target, err := d.fs.ReadLink(path)
		if err != nil {
			return fmt.Errorf("failed to read symlink target: %v", err)
		}
		w.Header().Set("X-LinkTarget", target)
		w.Header().Set("Content-Type", "application/x-symlink")
		w.Header().Set("Content-Length", "0")
	default:
		// Other files has no body and any specific headers
		w.Header().Set("Content-Length", "0")
	}

	return nil
}

func (d *FilesystemExporter) writeAttributes(w http.ResponseWriter, r *http.Request, path string, fi fs.FileInfo) error {
	mask := getRequestedAttributes(r.URL)
	attrs, err := d.prepareAttributes(mask, path, fi)
	if err != nil {
		return err
	}

	for k, v := range attrs {
		headerName, ok := mapAttributesToHeader[k]
		if !ok {
			err := fmt.Errorf("invalid attribute name: %v", k)
			d.logger.Error("failed to write attributes headers", "error", err)
			return err
		}

		w.Header().Set(headerName, fmt.Sprint(v))
	}

	return nil
}

func (d *FilesystemExporter) prepareAttributes(mask attributesMask, path string, fi fs.FileInfo) (map[string]any, error) {
	attrs := map[string]any{}

	// NOTE: currently stats are always written, don't check requested attributes
	if err := d.prepareAttributesStat(attrs, path, fi); err != nil {
		return nil, err
	}

	if mask.hashMd5 {
		if err := d.prepareAttributesMd5(attrs, path, fi); err != nil {
			return nil, err
		}
	}

	return attrs, nil
}

func (d *FilesystemExporter) prepareAttributesStat(attrs map[string]any, path string, fi fs.FileInfo) error {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		err := fmt.Errorf("failed to get GID and UID of file %s", path)
		d.logger.Error(err.Error())
		return err
	}

	attrs["modtime"] = fi.ModTime().Format(time.RFC3339)
	attrs["gid"] = stat.Gid
	attrs["uid"] = stat.Uid
	attrs["permissions"] = fmt.Sprintf("%#o", uint32(fi.Mode().Perm()))

	return nil
}

func (d *FilesystemExporter) prepareAttributesMd5(attrs map[string]any, path string, fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		// silently do nothing
		// if we list dir with md5 required, we need to calc md5  only for regular files and skip other
		return nil
	}

	f, err := d.fs.Open(path)
	if err != nil {
		d.logger.Error("failed to open file to calc checksum:", "error", err)
		return err
	}

	defer f.Close()

	hash := md5.New()
	_, err = io.Copy(hash, f)
	if err != nil {
		d.logger.Error("failed to calculate checksum:", "error", err)
		return err
	}
	attrs["hash.md5"] = fmt.Sprintf("%x", hash.Sum(nil))

	return nil
}

func (d *FilesystemExporter) HandleHeadMethod(w http.ResponseWriter, r *http.Request, requestedType ItemType) error {
	var httpErr *httpiohelpers.HTTPError

	_, err := d.prepareHead(w, r, requestedType)
	if errors.As(err, &httpErr) {
		return err
	} else if err != nil {
		d.logger.Error("failed to process requested file", "url", r.URL, "error", err)
		return err
	}

	return nil
}

func (d *FilesystemExporter) HandleGetMethod(w http.ResponseWriter, r *http.Request, requestedType ItemType) error {
	var httpErr *httpiohelpers.HTTPError

	path, err := d.prepareHead(w, r, requestedType)
	if errors.As(err, &httpErr) {
		return err
	} else if err != nil {
		d.logger.Error("failed to process requested file", "url", r.URL, "error", err)
		return err
	}

	fi, err := d.fs.Lstat(path)
	if err != nil {
		// We've already got stat of the file, so file exists. Therefore, any subsequeny
		// file errors are considered as internal server error
		d.logger.Error("failed to get file info", "err", err)
		return err
	}

	switch {
	case fi.Mode().IsDir():
		err := d.listDir(w, r, path)
		if err != nil {
			d.logger.Error("failed to list dir", "error", err)
			return err
		}
	case fi.Mode().IsRegular():
		err := d.sendFile(w, r, path, fi)
		if err != nil {
			d.logger.Error("failed to send dir", "error", err)
			return err
		}
	}

	return nil
}

func (d *FilesystemExporter) sendFile(w http.ResponseWriter, r *http.Request, path string, fi fs.FileInfo) error {
	file, err := d.fs.Open(path)
	if err != nil {
		d.logger.Error("failed to open file to serve", "error", err)
		return err
	}

	defer file.Close()

	http.ServeContent(w, r, "", fi.ModTime(), file)
	return nil
}

func (d *FilesystemExporter) listDir(w http.ResponseWriter, r *http.Request, path string) error {
	dir, err := d.fs.Open(path)
	if err != nil {
		d.logger.Error("failed to open dir to list", "error", err)
		return err
	}

	defer dir.Close()

	encoder := json.NewEncoder(w)
	attributesMask := getRequestedAttributes(r.URL)

	// Open response Json
	_, err = w.Write([]byte(`{"apiVersion": "v1", "items": [`))
	if err != nil {
		d.logger.Error("failed to write response json", "error", err)
		return err
	}

	fileCounter := 0
	for {
		entries, err := dir.ReadDir(dirListChunkSize)
		if err != nil && len(entries) == 0 {
			break
		}

		for _, dirEntry := range entries {
			itemPath := filepath.Join(path, dirEntry.Name())
			item, err := d.resolveItem(itemPath, dirEntry, attributesMask)
			if err != nil {
				return err
			}

			// add comma before second and subsequent items in list
			if fileCounter > 0 {
				_, err = w.Write([]byte(","))
				if err != nil {
					d.logger.Error("failed to write response json", "error", err)
					return err
				}
			}

			if err := encoder.Encode(item); err != nil {
				d.logger.Error("failed to serialize item", "error", err)
				return err
			}

			fileCounter++
		}
	}

	// Close Json
	_, err = w.Write([]byte("]}\n"))
	if err != nil {
		d.logger.Error("failed to write response json", "error", err)
		return err
	}

	return nil
}

func (d *FilesystemExporter) resolveItem(path string, entry fs.DirEntry, attributesMask attributesMask) (Item, error) {
	var item Item

	fileInfo, err := entry.Info()
	if err != nil {
		d.logger.Error("failed to get file info", "path", path, "error", err)
		return Item{}, err
	}

	attributes, err := d.prepareAttributes(attributesMask, path, fileInfo)
	if err != nil {
		d.logger.Error("Failed to get attributes of file", "path", path, "error", err)
		return Item{}, err
	}

	escapedPath, err := d.getFileURI(path, fileInfo)
	if err != nil {
		return Item{}, err
	}

	switch {
	case isSymlink(fileInfo.Mode()):
		itemType := link
		targetPath, err := d.fs.ReadLink(path)
		if err != nil {
			// TODO: why do we consider this situaltion as not error?
			itemType = linkErr
		}

		item = Item{
			Name:       fileInfo.Name(),
			Type:       string(itemType),
			URI:        escapedPath,
			TargetPath: targetPath,
			Attributes: attributes,
		}

	case fileInfo.Mode().IsRegular():
		attributes["size"] = fileInfo.Size()
		item = Item{
			Name:       fileInfo.Name(),
			Type:       string(file),
			URI:        escapedPath,
			Attributes: attributes,
		}

	case fileInfo.Mode().IsDir():
		item = Item{
			Name:       fileInfo.Name(),
			Type:       string(directory),
			URI:        escapedPath,
			Attributes: attributes,
		}
	default:
		item = Item{
			Name:       fileInfo.Name(),
			Type:       string(other),
			URI:        escapedPath,
			Attributes: attributes,
		}
	}

	return item, nil
}

func (d *FilesystemExporter) getFileURI(path string, fi fs.FileInfo) (string, error) {
	relPath, err := filepath.Rel(d.Root, path)
	if err != nil {
		d.logger.Error("Failed to convert absolute path to relative", "path", path, "base", d.Root, "error", err)
		return "", err
	}

	pathURL := url.URL{Path: relPath}
	escapedPath := pathURL.EscapedPath()

	if fi.IsDir() {
		escapedPath += "/"
	}

	return escapedPath, nil
}

func isSymlink(fi fs.FileMode) bool {
	return fi&fs.ModeSymlink != 0
}

func modeToType(mode fs.FileMode) ItemType {
	switch {
	case mode.IsDir():
		return directory
	case mode.IsRegular():
		return file
	case isSymlink(mode):
		return link
	}

	return other
}

func WrongPrefixBlockInformer(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "VolumeMode: Filesystem. Not supported downloading raw block. Use /api/v1/files", http.StatusBadRequest)
}

func WrongPrefixFilesInformer(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "Path is empty", http.StatusBadRequest)
}
