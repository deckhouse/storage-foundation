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

package shutdown

import (
	"errors"
	"sync"
	"syscall"
	"time"
)

type State int

const (
	Unknown State = iota
	Running
	Shutdown
	Blocked
)

func (s State) String() string {
	switch s {
	case Running:
		return "running"
	case Shutdown:
		return "shutdown"
	case Blocked:
		return "blocked"
	default:
		return "unknown"
	}
}

func Supported(fstype string) bool {
	return fstype == "xfs"
}

type StatFunc func(path string) error

func Stat(path string) error {
	var st syscall.Stat_t
	return syscall.Stat(path, &st)
}

type Prober struct {
	stat    StatFunc
	timeout time.Duration

	mu       sync.Mutex
	inFlight map[string]chan error
}

func NewProber(stat StatFunc, timeout time.Duration) *Prober {
	return &Prober{stat: stat, timeout: timeout, inFlight: map[string]chan error{}}
}

type Result struct {
	State State
	Err   error
}

func (p *Prober) Probe(path string) (State, error) {
	r := p.ProbeAll([]string{path})[path]
	return r.State, r.Err
}

func (p *Prober) ProbeAll(paths []string) map[string]Result {
	waits := make(map[string]chan error, len(paths))
	p.mu.Lock()
	for _, path := range paths {
		if _, seen := waits[path]; seen {
			continue
		}
		done, busy := p.inFlight[path]
		if !busy {
			done = make(chan error, 1)
			p.inFlight[path] = done
			go func(path string, done chan<- error) {
				done <- p.stat(path)
			}(path, done)
		}
		waits[path] = done
	}
	p.mu.Unlock()

	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	results := make(map[string]Result, len(waits))
	expired := false
	for path, done := range waits {
		if !expired {
			select {
			case err := <-done:
				results[path] = p.finish(path, err)
				continue
			case <-timer.C:
				expired = true
			}
		}
		select {
		case err := <-done:
			results[path] = p.finish(path, err)
		default:
			results[path] = Result{State: Blocked}
		}
	}
	return results
}

func (p *Prober) finish(path string, err error) Result {
	p.mu.Lock()
	delete(p.inFlight, path)
	p.mu.Unlock()
	state, err := classify(err)
	return Result{State: state, Err: err}
}

func (p *Prober) InFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inFlight)
}

func classify(err error) (State, error) {
	switch {
	case err == nil:
		return Running, nil
	case errors.Is(err, syscall.EIO):
		return Shutdown, nil
	default:
		return Unknown, err
	}
}
