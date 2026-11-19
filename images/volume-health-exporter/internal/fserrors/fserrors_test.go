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
	"os"
	"path/filepath"
	"testing"
)

func sysTree(t *testing.T, devices map[string]string, counts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dev", "block"), 0o755); err != nil {
		t.Fatal(err)
	}
	for majMin, name := range devices {
		if err := os.Symlink("../../devices/virtual/block/"+name, filepath.Join(dir, "dev", "block", majMin)); err != nil {
			t.Fatal(err)
		}
	}
	for name, count := range counts {
		d := filepath.Join(dir, "fs", "ext4", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "errors_count"), []byte(count), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCount(t *testing.T) {
	sys := sysTree(t,
		map[string]string{"252:16": "dm-3", "8:17": "sdb1", "252:32": "rbd1", "43:0": "nbd0"},
		map[string]string{"dm-3": "3\n", "sdb1": "0\n", "nbd0": "many\n"},
	)

	tests := []struct {
		name         string
		fstype       string
		major, minor int
		want         uint64
		wantOK       bool
		wantErr      bool
	}{
		{name: "ext4 with errors", fstype: "ext4", major: 252, minor: 16, want: 3, wantOK: true},
		{name: "ext3 on a partition without errors", fstype: "ext3", major: 8, minor: 17, want: 0, wantOK: true},
		{name: "ext4 without an errors_count file", fstype: "ext4", major: 252, minor: 32},
		{name: "unknown block device", fstype: "ext4", major: 252, minor: 99},
		{name: "xfs is not read", fstype: "xfs", major: 252, minor: 16},
		{name: "nfs has no block device", fstype: "nfs4", major: 0, minor: 77},
		{name: "broken errors_count", fstype: "ext4", major: 43, minor: 0, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := Count(sys, tt.fstype, tt.major, tt.minor)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Count() error = %v; want error %v", err, tt.wantErr)
			}
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("Count() = %d, %v; want %d, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
