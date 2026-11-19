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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "status.json")
	maxAge := 90 * time.Second

	check := func(probe Probe, at time.Time, wantOK bool) {
		t.Helper()
		err := Check(path, probe, maxAge, at)
		if (err == nil) != wantOK {
			t.Errorf("Check(%s) at +%s = %v; want ok %v", probe, at.Sub(start), err, wantOK)
		}
	}

	check(Liveness, start, false)
	check(Readiness, start, false)

	r, err := NewRecorder(path, start)
	if err != nil {
		t.Fatal(err)
	}
	check(Readiness, start, false)
	check(Liveness, start.Add(maxAge), true)
	check(Liveness, start.Add(maxAge+time.Second), false)

	if err := r.Succeeded(start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	check(Readiness, start.Add(time.Minute), true)
	check(Liveness, start.Add(time.Minute+maxAge), true)
	check(Liveness, start.Add(time.Minute+maxAge+time.Second), false)
	check(Readiness, start.Add(time.Hour), true)

	if err := r.Succeeded(start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	check(Liveness, start.Add(time.Hour), true)

	if _, err := NewRecorder(path, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check(Readiness, start.Add(2*time.Hour), false)
	check(Liveness, start.Add(2*time.Hour), true)

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("status directory holds %d entries; want only the status file", len(entries))
	}
}

func TestCheckBrokenStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, Liveness, time.Minute, time.Now()); err == nil {
		t.Error("Check() of a broken status file succeeded")
	}
	if _, err := NewRecorder(path, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, Probe("startup"), time.Minute, time.Now()); err == nil {
		t.Error("Check() of an unknown probe succeeded")
	}
}
