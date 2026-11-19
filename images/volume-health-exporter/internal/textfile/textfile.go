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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/deckhouse/storage-foundation/images/volume-health-exporter/internal/volumes"
)

const (
	ReadOnlyMetric         = "storage_foundation_volume_read_only"
	FilesystemErrorsMetric = "storage_foundation_volume_filesystem_errors"
	ShutdownMetric         = "storage_foundation_volume_filesystem_shutdown"
)

func Write(path string, vols []volumes.Volume) error {
	readOnly := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: ReadOnlyMetric,
		Help: "1 if the filesystem of a CSI volume published into a pod has turned read-only while the mount itself is writable, 0 if the filesystem is writable.",
	}, []string{"persistentvolume", "driver", "pod_uid", "fstype"})

	filesystemErrors := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: FilesystemErrorsMetric,
		Help: "Number of errors the ext4 filesystem of a CSI volume a pod mounts writable has recorded in its superblock since it was last checked with fsck.",
	}, []string{"persistentvolume", "driver", "fstype"})

	shutdown := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: ShutdownMetric,
		Help: "1 if the XFS filesystem of a CSI volume a pod mounts writable has shut down, 0 if it is running.",
	}, []string{"persistentvolume", "driver", "fstype"})

	for _, v := range vols {
		if v.Shutdown != nil {
			value := 0.0
			if *v.Shutdown {
				value = 1
			}
			shutdown.WithLabelValues(v.PersistentVolume, v.Driver, v.FSType).Set(value)
		}
		value := 0.0
		if v.ReadOnly {
			value = 1
		}
		readOnly.WithLabelValues(v.PersistentVolume, v.Driver, v.PodUID, v.FSType).Set(value)
		if v.FilesystemErrors != nil {
			filesystemErrors.WithLabelValues(v.PersistentVolume, v.Driver, v.FSType).Set(float64(*v.FilesystemErrors))
		}
	}

	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(readOnly); err != nil {
		return err
	}
	if err := registry.Register(filesystemErrors); err != nil {
		return err
	}
	if err := registry.Register(shutdown); err != nil {
		return err
	}
	return prometheus.WriteToTextfile(path, registry)
}

func RemoveLeftovers(path string) error {
	dir, base := filepath.Split(path)
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		suffix, ok := strings.CutPrefix(e.Name(), base)
		if !ok || suffix == "" || !e.Type().IsRegular() || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
