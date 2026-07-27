# Fuzzing

Fuzzing harness for the `data-exporter` HTTP handlers.

## What is fuzzed

Two targets in
[images/data-exporter/cmd/handler_fuzz_test.go](../../images/data-exporter/cmd/handler_fuzz_test.go),
one per exporter, so that each keeps its own corpus and coverage signal and a failure names
the exporter that produced it:

- `FuzzExportFilesystemHandler` — filesystem mode, exported root `/sharedir`
- `FuzzExportBlockHandler` — block mode, exported root `/block`

Each drives its exporter through the real `http.ServeMux` and authorization middleware,
fuzzing nine inputs per iteration:

- the authorization scheme (`basic`, `bearer`, or an arbitrary header value),
- basic credentials, bearer token, and a raw `Authorization` header,
- the request URL,
- the HTTP method (an index into nine methods, unsupported ones included),
- the request body,
- `failSeed` and `failProb`, which drive probabilistic I/O failure injection.

The filesystem is `mockfs` from `sds-common-lib`, populated with a deliberately hostile
tree: symlinks pointing outside the exported root (`dir_link -> /outer_dir`,
`file_link -> /foo/bar`), a dangling link, and a block device. Every file holds a marker
string instead of plausible data, which is what makes the containment oracle below possible.

The Kubernetes authorizer is a hand-written always-allow stub, so the targets exercise
routing, URL parsing and path containment rather than authorization decisions. It is not a
gomock: the generated mock calls `TestReporter.Helper` on every invocation and `*testing.F`
methods panic inside a fuzz target, so a gomock controller would have to be rebuilt on every
iteration — measured at ~9.5us and 49 allocations for no oracle value.

TTL control and the import handlers are out of scope; see [Not fuzzed](#not-fuzzed).

## What is checked

The targets assert properties, not just the absence of a crash. Beyond panics, hangs (Go
stops a worker that goes quiet for more than a second) and the response invariants below,
every iteration re-checks that the fixture is unchanged — the export handlers only read, and
this turns that assumption into a checked invariant rather than a comment.

| Property | Rationale |
| --- | --- |
| No response body contains a marker belonging to a file outside the exported root | The exporter's contract is that a path never escapes the exported root and that symlinks are followed neither at the end nor in the middle of a path. Markers are deliberately not path-shaped, so a symlink response legitimately reporting its target path is not mistaken for leaked content. |
| No 5xx when `failProb == 0` | Without an injected filesystem failure, every rejection is caused by the request, which is a 4xx. Turning client input into a server error hides the cause and burns error budget. |
| A 2xx implies the method was GET or HEAD | Those are the methods the exporter serves; the wrong-prefix informers reject everything else with a 400. |
| A 2xx implies a non-empty `Authorization` header | `authorization.Authorize` documents that a missing or unusable credential is a 401. |

The no-5xx invariant is what caught the unsupported-credential path returning 500 instead of
401 (`Authorization: Basic ...`), now fixed in
[internal/authorization/auth.go](../../images/data-exporter/internal/authorization/auth.go)
and pinned by seeds in the corpus plus
[internal/authorization/auth_test.go](../../images/data-exporter/internal/authorization/auth_test.go).
Note that it only applies with injection off, so a seed exercising a rejected credential has
to set `failProb` to 0 to be checked at all — the fuzzer had not reached that combination on
its own in ~340k iterations.

## Running

The whole pipeline, with the defaults tuned for a dedicated long run:

```sh
make -C hack/fuzzing all
```

Stages can also be run individually:

```sh
make -C hack/fuzzing fuzz FUZZ_TIME=48h DRY_TIME=2h PARALLEL=4
make -C hack/fuzzing fuzz TARGETS=FuzzExportBlockHandler
make -C hack/fuzzing race
make -C hack/fuzzing promote
make -C hack/fuzzing coverage
make -C hack/fuzzing archive
make -C hack/fuzzing help
```

| Variable    | Default               | Meaning                                                         |
| ----------- | --------------------- | --------------------------------------------------------------- |
| `TARGETS`   | both exporter targets | Fuzz targets to run.                                            |
| `FUZZ_TIME` | `48h`                 | Budget passed to `-fuzztime`, **per target**.                    |
| `DRY_TIME`  | `2h`                  | Stop a target early after this long without a new corpus entry.  |
| `PARALLEL`  | `4`                   | Fuzzing workers.                                                |

`go test -fuzz` takes a pattern that must match exactly one target, so the targets are fuzzed
one after another and a default full run takes `2 x FUZZ_TIME`.

This is a long manual run, not a CI job. Start it inside `tmux` when working over SSH.
`Ctrl+C` stops the fuzzer and every worker cleanly.

### Stages

1. **fuzz** — builds `runner/` and runs each target under it. Corpora land in
   `images/data-exporter/cmd/.fuzzcache/<target>`, the combined log in `out/fuzz.log`, the
   per-target exit status in `out/fuzz_status.txt`. A failing target neither aborts the
   pipeline nor skips the remaining targets: the reproducer and the log are what the later
   stages exist to collect.
2. **race** — replays the seed corpus concurrently under the race detector. The fuzz targets
   serve one request per iteration, so they cannot observe a race between concurrent
   requests, which is a failure mode this exporter has been fixed for before. Not part of
   `all`, and not covered by CI's plain `go test ./...`, so run it after touching the
   handlers. Failure injection stays off there: `mockfs` carries no synchronisation and
   `ProbabilityFailer` holds an unsynchronised `*rand.Rand`, so injecting would report a race
   inside the mock rather than inside the handler.
3. **promote** — copies each generated corpus into
   `images/data-exporter/cmd/testdata/fuzz/<target>`, where a plain `go test` replays it.
4. **coverage** — replays the promoted corpora with `-coverpkg=./...` in one profile and
   writes `out/coverage.html`, `out/coverage_func.txt`, `out/coverage.txt`. Implies
   `promote`. Coverage is measured here rather than during fuzzing, so the fuzzer itself runs
   without statement counters slowing it down.
5. **archive** — packs corpora, log, coverage report and a `summary.txt` (date, Go version,
   commit, per-target exit status) into `fuzz_report-<timestamp>.tar.gz`.

`make clean` removes the generated artifacts but keeps the promoted corpora in `testdata`;
`make clean-corpus` removes those too.

## When the fuzzer finds something

`go test -fuzz` writes the failing input to
`images/data-exporter/cmd/testdata/fuzz/<target>/` itself. Reproduce it with:

```sh
cd images/data-exporter
go test ./cmd -run 'FuzzExportFilesystemHandler/<file>'
```

Committing that file turns the crash into a regression test — CI runs `go test ./...` over
every image, which replays whatever sits in `testdata`.

## Corpus policy

The seed corpus lives in code, in `seedCorpus()`, and is the part worth curating: it is
reviewable, it documents which shapes matter, and CI replays it on every PR. Add a seed
whenever a run finds an input class worth keeping, as the credential seeds above do.

The machine-generated corpus is deliberately **not** committed. It is thousands of opaque
entries whose value is speeding up the next campaign, not describing behaviour, and Go's
tooling minimizes failing inputs rather than corpora, so there is no compact form to commit.
It survives in `.fuzzcache` between local runs and in the archive tarball. Two things are
worth committing: a reproducer for a real failure, and a seed distilled from it.

## Not fuzzed

- **TTL control** — it owns goroutines and mutable state shared across iterations, which
  produced false positives when it was included.
- **The import handlers** (`internal/import_block`, `internal/import_filesystem`) — they
  cannot be fuzzed safely as they stand. `fsext.FS` has no write operations at all, so the
  handlers reach past the injected filesystem and call `os.OpenFile`, `os.Rename`,
  `os.Chmod`, `os.Chown` and `os.Chtimes` directly. Driving them from a fuzzer would create,
  rename and chown **real files on the host** at paths derived from fuzzer input, and the
  mock fixture would be invisible to those calls anyway. Extending `fsext.FS` with the write
  operations and routing the import handlers through it is the prerequisite; after that they
  fit the existing pattern, except that the fixture has to be rebuilt per iteration (measured
  at ~7us) because writes would otherwise leak state between iterations, and the Kubernetes
  client has to be built once (~995us, far too slow per iteration) with the `DataImport`
  object reset between iterations.

## Debugging a fuzz target in VSCode

1. Add these configurations to `.vscode/launch.json`, replacing `$(go env GOCACHE)` with its
   actual value and the target name with the one being debugged:

    ```json
    {
        "version": "0.2.0",
        "configurations": [
            {
                "name": "Attach Fuzz Worker",
                "type": "go",
                "request": "attach",
                "mode": "local",
                "processId": "-test.fuzzworker",
                "preLaunchTask": "wait for proc"
            },
            {
                "name": "Launch Fuzz Coordinator",
                "type": "go",
                "request": "launch",
                "mode": "test",
                "program": "${workspaceFolder}/images/data-exporter/cmd",
                "console": "integratedTerminal",
                "args": [
                    "-test.fuzz=FuzzExportFilesystemHandler",
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

2. Run `Launch Fuzz Coordinator`.
3. Allow attaching to a running process: `echo "0" | sudo tee /proc/sys/kernel/yama/ptrace_scope`.
4. Run `Attach Fuzz Worker` and pick the fuzz worker process from the list.
5. Restore the setting: `echo "1" | sudo tee /proc/sys/kernel/yama/ptrace_scope`.

Note that `-test.fuzzcachedir` is resolved relative to the package directory, not the shell's
working directory; the harness passes an absolute path for that reason.

## Adding a target

The scripts assume every fuzz target lives in `<module>/cmd`, because coverage is collected
for the whole module while the targets sit in one package. Add the target to `TARGETS` in the
[Makefile](Makefile); adding one elsewhere means teaching `MODULE_DIR` about more than one
location.

New targets whose seed corpus contains Cyrillic (as these do, as adversarial path input) need
an entry under `no-cyrillic.exclude-rules.files` in [.dmtlint.yaml](../../.dmtlint.yaml).
