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

package volumes

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/moby/sys/mountinfo"
)

const (
	csiPluginDir       = "kubernetes.io~csi"
	volumeDataFileName = "vol_data.json"
	ephemeralMode      = "Ephemeral"
)

type Volume struct {
	PodUID           string
	PersistentVolume string
	Mountpoint       string
	Driver           string
	FSType           string
	ReadOnly         bool
	Major            int
	Minor            int
	FilesystemErrors *uint64
	Shutdown         *bool
}

type volumeData struct {
	SpecVolID           string `json:"specVolID"`
	DriverName          string `json:"driverName"`
	VolumeLifecycleMode string `json:"volumeLifecycleMode"`
}

type ReadFileFunc func(name string) ([]byte, error)

func Scan(mounts []*mountinfo.Info, kubeletRootDir string, readFile ReadFileFunc) ([]Volume, []error) {
	root := filepath.Clean(kubeletRootDir)
	podsDir := filepath.Join(root, "pods")
	stagingDir := filepath.Join(root, "plugins", "kubernetes.io", "csi")

	stagedReadOnly := make(map[device]bool)
	for _, m := range mounts {
		if isStagingPath(stagingDir, m.Mountpoint) && hasOption(m.Options, "ro") {
			stagedReadOnly[device{m.Major, m.Minor}] = true
		}
	}

	var (
		result []Volume
		errs   []error
	)
	for _, m := range topmost(mounts) {
		podUID, dir, ok := parsePublishPath(podsDir, m.Mountpoint)
		if !ok {
			continue
		}
		if hasOption(m.Options, "ro") {
			continue
		}
		if stagedReadOnly[device{m.Major, m.Minor}] {
			continue
		}

		dataPath := filepath.Join(podsDir, podUID, "volumes", csiPluginDir, dir, volumeDataFileName)
		raw, err := readFile(dataPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", dataPath, err))
			continue
		}
		var data volumeData
		if err := json.Unmarshal(raw, &data); err != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", dataPath, err))
			continue
		}
		if data.VolumeLifecycleMode == ephemeralMode {
			continue
		}

		pv := data.SpecVolID
		if pv == "" {
			pv = dir
		}
		result = append(result, Volume{
			PodUID:           podUID,
			PersistentVolume: pv,
			Mountpoint:       m.Mountpoint,
			Driver:           data.DriverName,
			FSType:           m.FSType,
			ReadOnly:         hasOption(m.VFSOptions, "ro"),
			Major:            m.Major,
			Minor:            m.Minor,
		})
	}

	slices.SortFunc(result, func(a, b Volume) int {
		return cmp.Or(cmp.Compare(a.PersistentVolume, b.PersistentVolume), cmp.Compare(a.PodUID, b.PodUID))
	})
	slices.SortFunc(errs, func(a, b error) int { return cmp.Compare(a.Error(), b.Error()) })
	return result, errs
}

type device struct{ major, minor int }

func topmost(mounts []*mountinfo.Info) []*mountinfo.Info {
	byPoint := make(map[string][]*mountinfo.Info)
	var points []string
	for _, m := range mounts {
		if _, seen := byPoint[m.Mountpoint]; !seen {
			points = append(points, m.Mountpoint)
		}
		byPoint[m.Mountpoint] = append(byPoint[m.Mountpoint], m)
	}

	result := make([]*mountinfo.Info, 0, len(points))
	for _, point := range points {
		stack := byPoint[point]
		covered := make(map[int]bool, len(stack))
		for _, m := range stack {
			covered[m.Parent] = true
		}
		top := stack[len(stack)-1]
		for _, m := range stack {
			if !covered[m.ID] {
				top = m
			}
		}
		result = append(result, top)
	}
	return result
}

func isStagingPath(stagingDir, mountpoint string) bool {
	rel, err := filepath.Rel(stagingDir, mountpoint)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	return filepath.Base(rel) == "globalmount"
}

func parsePublishPath(podsDir, mountpoint string) (podUID, dir string, ok bool) {
	rel, err := filepath.Rel(podsDir, mountpoint)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 5 || parts[1] != "volumes" || parts[2] != csiPluginDir || parts[4] != "mount" {
		return "", "", false
	}
	if parts[0] == "" || parts[0] == ".." || parts[3] == "" {
		return "", "", false
	}
	return parts[0], parts[3], true
}

func hasOption(options, name string) bool {
	return slices.Contains(strings.Split(options, ","), name)
}
