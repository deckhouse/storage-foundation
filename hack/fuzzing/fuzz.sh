#!/usr/bin/env bash

# Copyright 2026 Flant JSC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Runs the Go fuzz targets under the dry-period runner, one after another: `go test -fuzz`
# takes a pattern that must match exactly one target, so they cannot share a run.
#
# Coverage is deliberately not instrumented here: statement counters cost throughput on
# a multi-hour run, and `coverage.sh` measures the same thing afterwards over the corpus.

set -euo pipefail

MODULE_DIR="${1:?usage: fuzz.sh <module_dir> <fuzztime> <drytime> <parallel> <targets>}" # Go module to fuzz
FUZZ_TIME="${2:?fuzz time}"  # Fuzzing timeout per target
DRY_TIME="${3:?dry time}"    # Time without new inputs after which to stop
PARALLEL="${4:?parallel}"    # Number of parallel fuzzing workers
TARGETS="${5:?targets}"      # Space-separated list of fuzz targets to run

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${SCRIPT_DIR}/out"
RUNNER_DIR="${SCRIPT_DIR}/runner"
RUNNER_BIN="${RUNNER_DIR}/runner"
TEST_DIR="${MODULE_DIR}/cmd"
CACHE_DIR="${TEST_DIR}/.fuzzcache"

mkdir -p "${OUT_DIR}" "${CACHE_DIR}"

echo "[fuzz] Building dry-period runner"
(cd "${RUNNER_DIR}" && go build -o "${RUNNER_BIN}")

echo "[fuzz] Corpus cache: ${CACHE_DIR}"

# Truncate the combined log once, then append per target.
: > "${OUT_DIR}/fuzz.log"
: > "${OUT_DIR}/fuzz_status.txt"

FAILED_TARGETS=()

for target in ${TARGETS}; do
  echo "[fuzz] Running ${target} for ${FUZZ_TIME} (dry period ${DRY_TIME}, parallel=${PARALLEL})"
  echo "=== ${target} ===" >> "${OUT_DIR}/fuzz.log"

  # A crash found by the fuzzer must not abort the pipeline: the reproducer and the log are
  # exactly what the remaining stages are supposed to collect, and the other targets still
  # deserve their turn. Record the status instead.
  set +e
  (
    cd "${MODULE_DIR}" || exit 1
    "${RUNNER_BIN}" -t "${DRY_TIME}" -- go test \
      "${TEST_DIR}" \
      -run='^$' \
      -fuzz="^${target}\$" \
      -fuzztime="${FUZZ_TIME}" \
      -parallel="${PARALLEL}" \
      -test.fuzzcachedir="${CACHE_DIR}" \
      2>&1
  ) | tee -a "${OUT_DIR}/fuzz.log"
  status="${PIPESTATUS[0]}"
  set -e

  echo "${target} ${status}" >> "${OUT_DIR}/fuzz_status.txt"

  if [ "${status}" -ne 0 ]; then
    FAILED_TARGETS+=("${target} (exit ${status})")
  fi
done

echo "[fuzz] Log saved to ${OUT_DIR}/fuzz.log"

if [ "${#FAILED_TARGETS[@]}" -ne 0 ]; then
  echo "[fuzz] ================================================================"
  echo "[fuzz] FUZZING FAILED: ${FAILED_TARGETS[*]}"
  echo "[fuzz] If the fuzzer found a failing input, Go wrote the reproducer to"
  echo "[fuzz]   ${TEST_DIR}/testdata/fuzz/<target>/"
  echo "[fuzz] and it replays with: go test ./cmd -run '<target>/<file>'"
  echo "[fuzz] A build or setup error exits the same way — check the log above."
  echo "[fuzz] ================================================================"
else
  echo "[fuzz] Fuzzing finished without failures"
fi
