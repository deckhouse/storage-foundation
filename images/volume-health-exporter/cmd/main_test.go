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

package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deckhouse/storage-foundation/images/volume-health-exporter/internal/health"
)

const testUID = "0b6f6c1e-0d5a-4c39-9e1f-3a8c2f0d7a11"

type fixture struct {
	cfg       config
	mountinfo string
	publish   string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	kubelet := filepath.Join(root, "kubelet")
	volDir := filepath.Join(kubelet, "pods", testUID, "volumes", "kubernetes.io~csi", "pvc-a")
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"driverName":"rbd.csi.ceph.com","specVolID":"pvc-a","volumeLifecycleMode":"Persistent"}`
	if err := os.WriteFile(filepath.Join(volDir, "vol_data.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"textfile", "status"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := fixture{
		cfg: config{
			kubeletRootDir: kubelet,
			mountinfoPath:  filepath.Join(root, "mountinfo"),
			textfilePath:   filepath.Join(root, "textfile", "storage-foundation-volume-health.prom"),
			statusPath:     filepath.Join(root, "status", "status.json"),
			interval:       10 * time.Millisecond,
			statTimeout:    time.Second,
		},
		publish: filepath.Join(volDir, "mount"),
	}
	f.setMount(t, "ro")
	return f
}

func (f fixture) setMount(t *testing.T, superOptions string) {
	t.Helper()
	f.setMountFS(t, "ext4", superOptions)
}

func (f fixture) setMountFS(t *testing.T, fstype, superOptions string) {
	t.Helper()
	line := "3200 3150 252:16 / " + f.publish + " rw,relatime - " + fstype + " /dev/rbd0 " + superOptions + "\n"
	if err := os.WriteFile(f.cfg.mountinfoPath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestServeRemovesTheMetricsFileOnStop(t *testing.T) {
	f := newFixture(t)
	leftover := f.cfg.textfilePath + "123456"
	if err := os.WriteFile(leftover, []byte("stale 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, slog.New(slog.DiscardHandler), f.cfg) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(f.cfg.textfilePath)
		if err == nil && strings.Contains(string(raw), `persistentvolume="pvc-a"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the metrics file was not written: %v\n%s", err, raw)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := health.Check(f.cfg.statusPath, health.Readiness, f.cfg.maxAge(), time.Now()); err != nil {
		t.Errorf("readiness after a successful scan: %v", err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temporary file of an earlier run was left: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve() did not return after the context was cancelled")
	}
	if _, err := os.Stat(f.cfg.textfilePath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the metrics file is left after the stop: %v", err)
	}
}

func TestServeFailsWithoutStatusDirectory(t *testing.T) {
	f := newFixture(t)
	f.cfg.statusPath = filepath.Join(t.TempDir(), "absent", "status.json")
	if err := serve(context.Background(), slog.New(slog.DiscardHandler), f.cfg); err == nil {
		t.Error("serve() without a status directory succeeded")
	}
}

func TestScanLogsOnlyChanges(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	s := newScanner(slog.New(slog.NewTextHandler(&buf, nil)), f.cfg)

	count := func(msg string) int { return strings.Count(buf.String(), msg) }

	for range 3 {
		if !s.scan() {
			t.Fatal("scan() failed")
		}
	}
	if n := count("filesystem of a writable pod volume is read-only"); n != 1 {
		t.Errorf("read-only volume logged %d times over three scans; want 1:\n%s", n, buf.String())
	}

	f.setMount(t, "rw")
	s.scan()
	s.scan()
	if n := count("pod volume is no longer reported read-only"); n != 1 {
		t.Errorf("recovery logged %d times; want 1:\n%s", n, buf.String())
	}

	if err := os.Remove(filepath.Join(filepath.Dir(f.publish), "vol_data.json")); err != nil {
		t.Fatal(err)
	}
	s.scan()
	s.scan()
	if n := count("skip a pod volume mount"); n != 1 {
		t.Errorf("missing volume data logged %d times over two scans; want 1:\n%s", n, buf.String())
	}
}

func TestScanProbesXFSVolumes(t *testing.T) {
	f := newFixture(t)
	f.setMountFS(t, "xfs", "rw,attr2,inode64")
	if err := os.MkdirAll(f.publish, 0o755); err != nil {
		t.Fatal(err)
	}
	s := newScanner(slog.New(slog.DiscardHandler), f.cfg)
	if !s.scan() {
		t.Fatal("scan() failed")
	}
	raw, err := os.ReadFile(f.cfg.textfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `storage_foundation_volume_filesystem_shutdown{driver="rbd.csi.ceph.com",fstype="xfs",persistentvolume="pvc-a"} 0`) {
		t.Errorf("a running XFS volume has no shutdown series of 0:\n%s", raw)
	}

	if err := os.Remove(f.publish); err != nil {
		t.Fatal(err)
	}
	if !s.scan() {
		t.Fatal("scan() failed")
	}
	raw, err = os.ReadFile(f.cfg.textfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "storage_foundation_volume_filesystem_shutdown") {
		t.Errorf("an XFS volume whose stat fails with another error than EIO has a shutdown series:\n%s", raw)
	}
}
