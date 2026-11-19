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

package health

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Probe string

const (
	Liveness  Probe = "liveness"
	Readiness Probe = "readiness"
)

type State struct {
	Started     time.Time `json:"started"`
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
}

type Recorder struct {
	path  string
	state State
}

func NewRecorder(path string, now time.Time) (*Recorder, error) {
	r := &Recorder{path: path, state: State{Started: now}}
	return r, r.write()
}

func (r *Recorder) Succeeded(now time.Time) error {
	r.state.LastSuccess = now
	return r.write()
}

func (r *Recorder) write() error {
	raw, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), filepath.Base(r.path)+".tmp")
	if err != nil {
		return fmt.Errorf("write the status file %s: %w", r.path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write the status file %s: %w", r.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write the status file %s: %w", r.path, err)
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return fmt.Errorf("write the status file %s: %w", r.path, err)
	}
	return nil
}

func Check(path string, probe Probe, maxAge time.Duration, now time.Time) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the status file: %w", err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("parse the status file %s: %w", path, err)
	}

	switch probe {
	case Readiness:
		if s.LastSuccess.IsZero() {
			return errors.New("no successful scan yet")
		}
		return nil
	case Liveness:
		if s.LastSuccess.IsZero() {
			if age := now.Sub(s.Started); age > maxAge {
				return fmt.Errorf("no successful scan in %s since the start", age.Round(time.Second))
			}
			return nil
		}
		if age := now.Sub(s.LastSuccess); age > maxAge {
			return fmt.Errorf("the last successful scan was %s ago", age.Round(time.Second))
		}
		return nil
	default:
		return fmt.Errorf("unknown probe %q", probe)
	}
}
