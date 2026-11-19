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

package textfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/deckhouse/storage-foundation/images/volume-health-exporter/internal/volumes"
)

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage-foundation-volume-health.prom")
	if err := os.WriteFile(path, []byte("stale 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	three, zero := uint64(3), uint64(0)
	down, up := true, false
	vols := []volumes.Volume{
		{PodUID: "uid-a", PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", ReadOnly: true, FilesystemErrors: &three},
		{PodUID: "uid-c", PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", ReadOnly: true, FilesystemErrors: &three},
		{PodUID: "uid-b", PersistentVolume: "pvc-b", Driver: "nfs.csi.storage.deckhouse.io", FSType: "nfs4"},
		{PodUID: "uid-d", PersistentVolume: "pvc-d", Driver: "local.csi.storage.deckhouse.io", FSType: "ext4", FilesystemErrors: &zero},
		{PodUID: "uid-e", PersistentVolume: "pvc-e", Driver: "local.csi.storage.deckhouse.io", FSType: "xfs", Shutdown: &down},
		{PodUID: "uid-f", PersistentVolume: "pvc-e", Driver: "local.csi.storage.deckhouse.io", FSType: "xfs", Shutdown: &down},
		{PodUID: "uid-g", PersistentVolume: "pvc-g", Driver: "local.csi.storage.deckhouse.io", FSType: "xfs", Shutdown: &up},
	}
	if err := Write(path, vols); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("file mode = %o; want 644", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries; want only the metrics file", len(entries))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse written file: %v\n%s", err, raw)
	}
	if len(families) != 3 {
		t.Fatalf("written file holds %d metric families; want 3:\n%s", len(families), raw)
	}

	checkSeries(t, families, ReadOnlyMetric, []string{"persistentvolume", "driver", "pod_uid", "fstype"}, map[string]float64{
		"pvc-a|rbd.csi.ceph.com|uid-a|ext4":               1,
		"pvc-a|rbd.csi.ceph.com|uid-c|ext4":               1,
		"pvc-b|nfs.csi.storage.deckhouse.io|uid-b|nfs4":   0,
		"pvc-d|local.csi.storage.deckhouse.io|uid-d|ext4": 0,
		"pvc-e|local.csi.storage.deckhouse.io|uid-e|xfs":  0,
		"pvc-e|local.csi.storage.deckhouse.io|uid-f|xfs":  0,
		"pvc-g|local.csi.storage.deckhouse.io|uid-g|xfs":  0,
	})
	checkSeries(t, families, FilesystemErrorsMetric, []string{"persistentvolume", "driver", "fstype"}, map[string]float64{
		"pvc-a|rbd.csi.ceph.com|ext4":               3,
		"pvc-d|local.csi.storage.deckhouse.io|ext4": 0,
	})
	checkSeries(t, families, ShutdownMetric, []string{"persistentvolume", "driver", "fstype"}, map[string]float64{
		"pvc-e|local.csi.storage.deckhouse.io|xfs": 1,
		"pvc-g|local.csi.storage.deckhouse.io|xfs": 0,
	})
}

func checkSeries(t *testing.T, families map[string]*dto.MetricFamily, name string, labelNames []string, want map[string]float64) {
	t.Helper()
	family, ok := families[name]
	if !ok {
		t.Fatalf("written file has no %s", name)
	}
	got := map[string]float64{}
	for _, m := range family.GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if len(labels) != len(labelNames) {
			t.Errorf("%s series has labels %v; want only %v", name, labels, labelNames)
		}
		values := make([]string, 0, len(labelNames))
		for _, n := range labelNames {
			values = append(values, labels[n])
		}
		got[strings.Join(values, "|")] = m.GetGauge().GetValue()
	}
	if len(got) != len(want) {
		t.Fatalf("%s series = %v; want %v", name, got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s series %s = %v; want %v", name, k, g, v)
		}
	}
}

func TestWriteNoVolumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "storage-foundation-volume-health.prom")
	if err := os.WriteFile(path, []byte(ReadOnlyMetric+`{persistentvolume="gone"} 1`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, nil); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "gone") {
		t.Errorf("series of an unmounted volume survived a rewrite:\n%s", raw)
	}
}

func TestRemoveLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage-foundation-volume-health.prom")
	keep := []string{
		"storage-foundation-volume-health.prom",
		"storage-foundation-volume-health.prom.bak",
		"storage-foundation-volume-health.promx1",
		"registry-agent.prom",
		"registry-agent.prom123456",
	}
	drop := []string{
		"storage-foundation-volume-health.prom123456",
		"storage-foundation-volume-health.prom9",
	}
	for _, name := range append(append([]string{}, keep...), drop...) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "storage-foundation-volume-health.prom42"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep = append(keep, "storage-foundation-volume-health.prom42")

	if err := RemoveLeftovers(path); err != nil {
		t.Fatalf("RemoveLeftovers() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, name := range keep {
		if !got[name] {
			t.Errorf("%s was removed", name)
		}
	}
	for _, name := range drop {
		if got[name] {
			t.Errorf("%s was left", name)
		}
	}
}
