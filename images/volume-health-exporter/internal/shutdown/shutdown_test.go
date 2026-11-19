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
	"fmt"
	"io/fs"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestProbeStates(t *testing.T) {
	results := map[string]error{
		"/running":  nil,
		"/shutdown": &fs.PathError{Op: "stat", Path: "/shutdown", Err: syscall.EIO},
		"/denied":   &fs.PathError{Op: "stat", Path: "/denied", Err: syscall.EACCES},
		"/wrapped":  fmt.Errorf("stat: %w", syscall.EIO),
	}
	p := NewProber(func(path string) error { return results[path] }, time.Second)

	tests := []struct {
		path    string
		want    State
		wantErr bool
	}{
		{path: "/running", want: Running},
		{path: "/shutdown", want: Shutdown},
		{path: "/wrapped", want: Shutdown},
		{path: "/denied", want: Unknown, wantErr: true},
	}
	for _, tt := range tests {
		got, err := p.Probe(tt.path)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("Probe(%s) = %s, %v; want %s, error %v", tt.path, got, err, tt.want, tt.wantErr)
		}
	}
	if n := p.InFlight(); n != 0 {
		t.Errorf("%d calls left in flight after they all returned", n)
	}
}

func TestProbeDoesNotWaitForABlockedCall(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	p := NewProber(func(string) error {
		calls.Add(1)
		<-release
		return nil
	}, 20*time.Millisecond)

	for range 3 {
		start := time.Now()
		got, err := p.Probe("/hung")
		if got != Blocked || err != nil {
			t.Fatalf("Probe() of a hung call = %s, %v; want blocked", got, err)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("Probe() waited %s for a hung call", took)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("a hung path was called %d times; want one call in flight", n)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := p.Probe("/hung")
		if got == Running && err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Probe() after the call returned = %s, %v; want running", got, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the path was called %d times; want the result of the call in flight", n)
	}
}

func TestStat(t *testing.T) {
	if err := Stat(t.TempDir()); err != nil {
		t.Errorf("Stat() of a directory = %v", err)
	}
	state, err := classify(Stat("/nonexistent-volume-health-path"))
	if state != Unknown || err == nil {
		t.Errorf("Stat() of a missing path = %s, %v; want unknown with an error", state, err)
	}
}

func TestSupported(t *testing.T) {
	for fstype, want := range map[string]bool{"xfs": true, "ext4": false, "nfs4": false, "": false} {
		if got := Supported(fstype); got != want {
			t.Errorf("Supported(%q) = %v; want %v", fstype, got, want)
		}
	}
}

func TestProbeAllSharesOneDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	p := NewProber(func(path string) error {
		if path == "/fine" {
			return nil
		}
		<-release
		return nil
	}, 50*time.Millisecond)

	start := time.Now()
	got := p.ProbeAll([]string{"/hung-a", "/hung-b", "/hung-c", "/fine", "/fine"})
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("ProbeAll() of three hung paths took %s; want one shared timeout", took)
	}
	want := map[string]State{"/hung-a": Blocked, "/hung-b": Blocked, "/hung-c": Blocked, "/fine": Running}
	if len(got) != len(want) {
		t.Fatalf("ProbeAll() = %v; want %v", got, want)
	}
	for path, state := range want {
		if got[path].State != state {
			t.Errorf("ProbeAll()[%s] = %s; want %s", path, got[path].State, state)
		}
	}
	if n := p.InFlight(); n != 3 {
		t.Errorf("%d calls in flight; want the three hung ones", n)
	}
}
