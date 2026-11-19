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

package fserrors

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var ext4Types = map[string]bool{"ext2": true, "ext3": true, "ext4": true}

func Supported(fstype string) bool {
	return ext4Types[fstype]
}

func Count(sysDir string, fstype string, major, minor int) (uint64, bool, error) {
	if !Supported(fstype) {
		return 0, false, nil
	}
	link := filepath.Join(sysDir, "dev", "block", fmt.Sprintf("%d:%d", major, minor))
	target, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("resolve the block device %d:%d: %w", major, minor, err)
	}
	path := filepath.Join(sysDir, "fs", "ext4", filepath.Base(target), "errors_count")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read %s: %w", path, err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return n, true, nil
}
