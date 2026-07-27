# Fuzzing

Fuzzing harness for the `data-exporter` HTTP handlers.

## What is fuzzed

A single target, `FuzzHandler`, in
[images/data-exporter/cmd/handler_fuzz_test.go](../../images/data-exporter/cmd/handler_fuzz_test.go).
It drives the export handlers through their real `http.ServeMux` and authorization
middleware, fuzzing nine inputs per iteration:

- the authorization scheme (`basic`, `bearer`, or an arbitrary header value),
- basic credentials, bearer token, and a raw `Authorization` header,
- the request URL,
- the HTTP method (an index into nine methods, unsupported ones included),
- the request body,
- `failSeed` and `failProb`, which drive probabilistic I/O failure injection.

The filesystem is `mockfs` from `sds-common-lib`, populated with a deliberately hostile
tree: symlinks pointing outside the shared directory (`dir_link -> /outer_dir`,
`file_link -> /foo/bar`), a broken link, and a block device. Each iteration runs the same
request against both the filesystem exporter and the block exporter.

The Kubernetes authorizer is a gomock that always authorizes, so the target exercises
routing, URL parsing and path containment — not authorization decisions themselves.

TTL control is intentionally out of scope: it owns goroutines and mutable state shared
across iterations, which produced false positives when it was included.

## Running

The whole pipeline, with the defaults tuned for a dedicated long run:

```sh
make -C hack/fuzzing all
```

Stages can also be run individually:

```sh
make -C hack/fuzzing fuzz FUZZ_TIME=48h DRY_TIME=2h PARALLEL=4
make -C hack/fuzzing promote
make -C hack/fuzzing coverage
make -C hack/fuzzing archive
make -C hack/fuzzing help
```

| Variable    | Default | Meaning                                                  |
| ----------- | ------- | -------------------------------------------------------- |
| `FUZZ_TIME` | `48h`   | Total fuzzing budget passed to `-fuzztime`.              |
| `DRY_TIME`  | `2h`    | Stop early after this long without a new corpus entry.   |
| `PARALLEL`  | `4`     | Fuzzing workers.                                         |

This is a long manual run, not a CI job. Start it inside `tmux` when working over SSH.
`Ctrl+C` stops the fuzzer and every worker cleanly.

### Stages

1. **fuzz** — builds `runner/` and runs `go test -fuzz=FuzzHandler` under it. The corpus
   lands in `images/data-exporter/cmd/.fuzzcache`, the log in `out/fuzz.log`, the exit
   status in `out/fuzz_status.txt`. A failing input does not abort the pipeline: the
   reproducer and the log are what the later stages exist to collect.
2. **promote** — copies the generated corpus into
   `images/data-exporter/cmd/testdata/fuzz/FuzzHandler`, where a plain `go test` replays
   it. This is what makes a run reusable.
3. **coverage** — replays that corpus with `-coverpkg=./...` and writes
   `out/coverage.html`, `out/coverage_func.txt`, `out/coverage.txt`. Implies `promote`.
   Coverage is measured here rather than during fuzzing, so the fuzzer itself runs without
   statement counters slowing it down.
4. **archive** — packs corpus, log, coverage report and a `summary.txt` (date, Go version,
   commit, fuzz exit status) into `fuzz_report-<timestamp>.tar.gz`.

`make clean` removes the generated artifacts but keeps the promoted corpus in `testdata`;
`make clean-corpus` removes that too.

## When the fuzzer finds something

`go test -fuzz` writes the failing input to
`images/data-exporter/cmd/testdata/fuzz/FuzzHandler/` itself. Reproduce it with:

```sh
cd images/data-exporter
go test ./cmd -run 'FuzzHandler/<file>'
```

Committing that file turns the crash into a regression test — CI runs `go test ./...` over
every image, which replays the whole `testdata` corpus.

That cuts both ways when promoting a full run: CI replays every committed corpus entry
once per build tag (`ce ee se seplus csepro`). Commit a minimized corpus, not thousands of
raw entries; the archive tarball is the place for the full set.

## Debugging the fuzz target in VSCode

1. Add these configurations to `.vscode/launch.json`, replacing `$(go env GOCACHE)` with
   its actual value:

    ```json
    {
        "version": "0.2.0",
        "configurations": [
            {
                "name": "Attach FuzzHandler Worker",
                "type": "go",
                "request": "attach",
                "mode": "local",
                "processId": "-test.fuzzworker",
                "preLaunchTask": "wait for proc"
            },
            {
                "name": "Launch FuzzHandler Coordinator",
                "type": "go",
                "request": "launch",
                "mode": "test",
                "program": "${workspaceFolder}/images/data-exporter/cmd",
                "console": "integratedTerminal",
                "args": [
                    "-test.fuzz=FuzzHandler",
                    "-test.fuzzcachedir=$(go env GOCACHE)/fuzz/github.com/deckhouse/storage-foundation/images/data-exporter/cmd",
                    "-test.parallel=1"
                ],
                "buildFlags": [
                    "-cover",
                    "-coverpkg=./images/data-exporter/cmd",
                    "-covermode=atomic"
                ]
            }
        ]
    }
    ```

2. Run `Launch FuzzHandler Coordinator`.
3. Allow attaching to a running process: `echo "0" | sudo tee /proc/sys/kernel/yama/ptrace_scope`.
4. Run `Attach FuzzHandler Worker` and pick the fuzz worker process from the list.
5. Restore the setting: `echo "1" | sudo tee /proc/sys/kernel/yama/ptrace_scope`.

## Adding a target

The scripts assume every fuzz target lives in `<module>/cmd`, because coverage is
collected for the whole module while the target sits in one package. Adding a target
elsewhere means teaching `MODULE_DIR`/`TEST_NAME` in the Makefile about more than one
location.

New targets whose seed corpus contains Cyrillic (as `FuzzHandler`'s adversarial paths do)
need an entry under `no-cyrillic.exclude-rules.files` in
[.dmtlint.yaml](../../.dmtlint.yaml).
