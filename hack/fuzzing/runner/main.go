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

// Runs `go test -fuzz` and stops it once fuzzing goes dry.
//
// `go test -fuzz` only knows about a wall-clock budget (-fuzztime), so a run that stopped
// finding new coverage hours ago keeps burning the budget. This wrapper mirrors the child's
// output, watches the "(total: N)" counter the fuzzer prints, and interrupts the run once
// that counter has not grown for the given dry period.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func main() {
	dryStr := flag.String("t", "", "dry period threshold, e.g. 30m, 2h")
	grace := flag.Duration("grace", 10*time.Second, "grace period after SIGINT before SIGKILL")
	flag.Parse()

	if *dryStr == "" {
		fmt.Fprintln(os.Stderr, "usage: runner -t <duration> [options] -- <cmd> [args...]")
		os.Exit(2)
	}

	dryFor, err := time.ParseDuration(*dryStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid -t:", err)
		os.Exit(2)
	}

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "no command provided; usage: runner -t <duration> -- <cmd> [args...]")
		os.Exit(2)
	}

	cmd := exec.Command(args[0], args[1:]...)
	// Own process group, so a single signal reaches the fuzz workers too, not just `go test`.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipe stdout:", err)
		os.Exit(1)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipe stderr:", err)
		os.Exit(1)
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}

	pgid := cmd.Process.Pid

	// Forward incoming SIGINT/SIGTERM to the child group so Ctrl+C stops every worker.
	go func() {
		sigCh := make(chan os.Signal, 2)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		for s := range sigCh {
			if sig, ok := s.(syscall.Signal); ok {
				_ = syscall.Kill(-pgid, sig)
			}
		}
	}()

	updates := make(chan int, 16)
	done := make(chan error, 1)

	var readers sync.WaitGroup
	readers.Add(2)
	go streamAndParse(&readers, stdout, os.Stdout, updates)
	go streamAndParse(&readers, stderr, os.Stderr, updates)

	go func() {
		// Wait closes the pipes as soon as the child exits, so it must not run until both
		// readers have drained them; otherwise the tail of the fuzz log — the final counter
		// and any crash report — is lost exactly when it matters.
		readers.Wait()
		done <- cmd.Wait()
	}()

	lastTotal := -1
	var lastBump time.Time // zero until the first counter line arrives

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			exitWith(err)

		case t := <-updates:
			if t > lastTotal {
				lastTotal = t
				lastBump = time.Now()
			}

		case <-ticker.C:
			// Baseline coverage gathering emits no counter; don't count that as dry time.
			if lastBump.IsZero() || time.Since(lastBump) < dryFor {
				continue
			}

			fmt.Fprintf(os.Stderr, "[runner] no new inputs for %s, sending SIGINT\n", dryFor)
			_ = syscall.Kill(-pgid, syscall.SIGINT)

			select {
			case <-done:
				// Stopping on a dry period is the expected outcome, not a failure.
				os.Exit(0)
			case <-time.After(*grace):
				fmt.Fprintln(os.Stderr, "[runner] grace period exceeded, sending SIGKILL")
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				<-done
				os.Exit(0)
			}
		}
	}
}

// The fuzzer reports its corpus size as "new interesting: 12 (total: 46)".
var totalRe = regexp.MustCompile(`\(total:\s*([0-9]+)\)`)

func streamAndParse(wg *sync.WaitGroup, r io.Reader, mirror io.Writer, updates chan<- int) {
	defer wg.Done()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintln(mirror, line)

		m := totalRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}

		// Never block the mirror on a full channel: the reader is what keeps the log flowing.
		select {
		case updates <- n:
		default:
		}
	}

	// A line longer than the buffer stops the scanner. Keep draining so the child never
	// blocks on a full pipe, even though the tail is no longer parsed for counters.
	if sc.Err() != nil {
		_, _ = io.Copy(mirror, r)
	}
}

func exitWith(err error) {
	if err == nil {
		os.Exit(0)
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}

	fmt.Fprintln(os.Stderr, "[runner] wait:", err)
	os.Exit(1)
}
