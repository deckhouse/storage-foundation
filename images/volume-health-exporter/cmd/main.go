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
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/moby/sys/mountinfo"

	"github.com/deckhouse/storage-foundation/images/volume-health-exporter/internal/fserrors"
	"github.com/deckhouse/storage-foundation/images/volume-health-exporter/internal/health"
	"github.com/deckhouse/storage-foundation/images/volume-health-exporter/internal/shutdown"
	"github.com/deckhouse/storage-foundation/images/volume-health-exporter/internal/textfile"
	"github.com/deckhouse/storage-foundation/images/volume-health-exporter/internal/volumes"
)

type config struct {
	kubeletRootDir string
	mountinfoPath  string
	sysDir         string
	textfilePath   string
	statusPath     string
	interval       time.Duration
	statTimeout    time.Duration
	probe          string
}

func (c config) maxAge() time.Duration {
	return 3 * c.interval
}

func main() {
	var cfg config
	flag.StringVar(&cfg.kubeletRootDir, "kubelet-root-dir", "/var/lib/kubelet", "root directory of the kubelet on the node, mounted at the same path")
	flag.StringVar(&cfg.mountinfoPath, "mountinfo", "/proc/self/mountinfo", "mount table to read the pod volume mounts from")
	flag.StringVar(&cfg.sysDir, "sys-dir", "/sys", "sysfs to read the error counters of the filesystems from")
	flag.StringVar(&cfg.textfilePath, "textfile", "/var/run/node-exporter-textfile/storage-foundation-volume-health.prom", "file the node exporter textfile collector reads the metrics from")
	flag.StringVar(&cfg.statusPath, "status-file", "/run/volume-health-exporter/status.json", "file the exporter records its scans in for the probes")
	flag.DurationVar(&cfg.interval, "interval", 30*time.Second, "time between two scans")
	flag.DurationVar(&cfg.statTimeout, "stat-timeout", 2*time.Second, "longest time a scan waits for the stat calls that detect a shut down XFS filesystem")
	flag.StringVar(&cfg.probe, "probe", "", "check the status file for the liveness or readiness probe and exit instead of running")
	flag.Parse()

	if cfg.probe != "" {
		if err := health.Check(cfg.statusPath, health.Probe(cfg.probe), cfg.maxAge(), time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, cfg); err != nil {
		log.Error("stop", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, cfg config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serve(ctx, log, cfg)
}

func serve(ctx context.Context, log *slog.Logger, cfg config) error {
	recorder, err := health.NewRecorder(cfg.statusPath, time.Now())
	if err != nil {
		return err
	}
	if err := textfile.RemoveLeftovers(cfg.textfilePath); err != nil {
		log.Warn("remove the temporary files of an earlier run", "path", cfg.textfilePath, "error", err)
	}

	s := newScanner(log, cfg)
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for {
		if s.scan() {
			if err := recorder.Succeeded(time.Now()); err != nil {
				log.Error("record the scan", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			if err := os.Remove(cfg.textfilePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove the metrics file %s: %w", cfg.textfilePath, err)
			}
			return nil
		case <-ticker.C:
		}
	}
}

type scanner struct {
	log      *slog.Logger
	cfg      config
	readOnly map[string]volumes.Volume
	errors   map[string]uint64
	states   map[string]shutdown.State
	skipped  map[string]bool
	prober   *shutdown.Prober
}

func newScanner(log *slog.Logger, cfg config) *scanner {
	return &scanner{log: log, cfg: cfg, readOnly: map[string]volumes.Volume{}, errors: map[string]uint64{}, states: map[string]shutdown.State{}, skipped: map[string]bool{},
		prober: shutdown.NewProber(shutdown.Stat, cfg.statTimeout),
	}
}

func (s *scanner) scan() bool {
	mounts, err := readMounts(s.cfg.mountinfoPath)
	if err != nil {
		s.log.Error("read the mount table", "path", s.cfg.mountinfoPath, "error", err)
		return false
	}
	vols, errs := volumes.Scan(mounts, s.cfg.kubeletRootDir, os.ReadFile)
	for i := range vols {
		count, ok, err := fserrors.Count(s.cfg.sysDir, vols[i].FSType, vols[i].Major, vols[i].Minor)
		if err != nil {
			errs = append(errs, fmt.Errorf("read the error counter of the filesystem of %s: %w", vols[i].PersistentVolume, err))
			continue
		}
		if ok {
			vols[i].FilesystemErrors = &count
		}
	}
	errs = append(errs, s.probeShutdown(vols)...)
	s.logSkipped(errs)
	if err := textfile.Write(s.cfg.textfilePath, vols); err != nil {
		s.log.Error("write the metrics file", "path", s.cfg.textfilePath, "error", err)
		return false
	}
	s.logReadOnly(vols)
	s.logFilesystemErrors(vols)
	return true
}

func (s *scanner) probeShutdown(vols []volumes.Volume) []error {
	var paths []string
	for _, v := range vols {
		if shutdown.Supported(v.FSType) {
			paths = append(paths, v.Mountpoint)
		}
	}
	if len(paths) == 0 {
		s.states = map[string]shutdown.State{}
		return nil
	}

	results := s.prober.ProbeAll(paths)
	states := make(map[string]shutdown.State, len(results))
	var errs []error
	for i := range vols {
		r, ok := results[vols[i].Mountpoint]
		if !ok {
			continue
		}
		states[vols[i].Mountpoint] = r.State
		if prev := s.states[vols[i].Mountpoint]; prev != r.State && (r.State == shutdown.Shutdown || r.State == shutdown.Blocked || prev == shutdown.Shutdown || prev == shutdown.Blocked) {
			s.log.Warn("state of an XFS pod volume changed", "persistentvolume", vols[i].PersistentVolume, "driver", vols[i].Driver, "pod_uid", vols[i].PodUID, "from", prev.String(), "to", r.State.String())
		}
		switch r.State {
		case shutdown.Running:
			down := false
			vols[i].Shutdown = &down
		case shutdown.Shutdown:
			down := true
			vols[i].Shutdown = &down
		case shutdown.Unknown:
			errs = append(errs, fmt.Errorf("stat the XFS filesystem of %s: %w", vols[i].PersistentVolume, r.Err))
		}
	}
	s.states = states
	return errs
}

func (s *scanner) logFilesystemErrors(vols []volumes.Volume) {
	current := make(map[string]uint64)
	for _, v := range vols {
		if v.FilesystemErrors == nil || *v.FilesystemErrors == 0 {
			continue
		}
		key := v.Driver + "/" + v.PersistentVolume
		if _, seen := current[key]; seen {
			continue
		}
		current[key] = *v.FilesystemErrors
		if prev, known := s.errors[key]; !known || prev != *v.FilesystemErrors {
			s.log.Warn("filesystem of a pod volume has recorded errors", "persistentvolume", v.PersistentVolume, "driver", v.Driver, "fstype", v.FSType, "errors", *v.FilesystemErrors)
		}
	}
	s.errors = current
}

func (s *scanner) logSkipped(errs []error) {
	current := make(map[string]bool, len(errs))
	for _, err := range errs {
		msg := err.Error()
		current[msg] = true
		if !s.skipped[msg] {
			s.log.Warn("skip a pod volume mount", "error", msg)
		}
	}
	s.skipped = current
}

func (s *scanner) logReadOnly(vols []volumes.Volume) {
	current := make(map[string]volumes.Volume)
	for _, v := range vols {
		if !v.ReadOnly {
			continue
		}
		key := v.PodUID + "/" + v.PersistentVolume
		current[key] = v
		if _, known := s.readOnly[key]; !known {
			s.log.Warn("filesystem of a writable pod volume is read-only", "persistentvolume", v.PersistentVolume, "driver", v.Driver, "pod_uid", v.PodUID, "fstype", v.FSType)
		}
	}
	for key, v := range s.readOnly {
		if _, still := current[key]; !still {
			s.log.Info("pod volume is no longer reported read-only", "persistentvolume", v.PersistentVolume, "driver", v.Driver, "pod_uid", v.PodUID)
		}
	}
	s.readOnly = current
}

func readMounts(path string) ([]*mountinfo.Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return mountinfo.GetMountsFromReader(f, nil)
}
