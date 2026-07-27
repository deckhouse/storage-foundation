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

# Runs the Go fuzz target under the dry-period runner.
#
# Coverage is deliberately not instrumented here: statement counters cost throughput on
# a multi-hour run, and `coverage.sh` measures the same thing afterwards over the corpus.

set -euo pipefail

MODULE_DIR="${1:?usage: fuzz.sh <module_dir> <fuzztime> <drytime> <parallel> <test_name>}" # Go module to fuzz
FUZZ_TIME="${2:?fuzz time}"  # Total fuzzing timeout
DRY_TIME="${3:?dry time}"    # Time without new inputs after which to stop
PARALLEL="${4:?parallel}"    # Number of parallel fuzzing workers
TEST_NAME="${5:?test name}"  # Fuzz target to run

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${SCRIPT_DIR}/out"
RUNNER_DIR="${SCRIPT_DIR}/runner"
RUNNER_BIN="${RUNNER_DIR}/runner"
TEST_DIR="${MODULE_DIR}/cmd"
CACHE_DIR="${TEST_DIR}/.fuzzcache"

mkdir -p "${OUT_DIR}" "${CACHE_DIR}"

echo "[fuzz] Building dry-period runner"
(cd "${RUNNER_DIR}" && go build -o "${RUNNER_BIN}")

echo "[fuzz] Running ${TEST_NAME} for ${FUZZ_TIME} (dry period ${DRY_TIME}, parallel=${PARALLEL})"
echo "[fuzz] Corpus cache: ${CACHE_DIR}"

# A crash found by the fuzzer must not abort the pipeline: the reproducer and the log are
# exactly what the remaining stages are supposed to collect. Record the status instead.
set +e
(
  cd "${MODULE_DIR}" || exit 1
  "${RUNNER_BIN}" -t "${DRY_TIME}" -- go test \
    "${TEST_DIR}" \
    -run='^$' \
    -fuzz="${TEST_NAME}" \
    -fuzztime="${FUZZ_TIME}" \
    -parallel="${PARALLEL}" \
    -test.fuzzcachedir="${CACHE_DIR}" \
    2>&1
) | tee "${OUT_DIR}/fuzz.log"
FUZZ_STATUS="${PIPESTATUS[0]}"
set -e

echo "${FUZZ_STATUS}" > "${OUT_DIR}/fuzz_status.txt"
echo "[fuzz] Log saved to ${OUT_DIR}/fuzz.log"

if [ "${FUZZ_STATUS}" -ne 0 ]; then
  echo "[fuzz] ================================================================"
  echo "[fuzz] FUZZING FAILED (exit ${FUZZ_STATUS})"
  echo "[fuzz] If the fuzzer found a failing input, Go wrote the reproducer to"
  echo "[fuzz]   ${TEST_DIR}/testdata/fuzz/${TEST_NAME}/"
  echo "[fuzz] and it replays with: go test ./cmd -run '${TEST_NAME}/<file>'"
  echo "[fuzz] A build or setup error exits the same way — check the log above."
  echo "[fuzz] ================================================================"
else
  echo "[fuzz] Fuzzing finished without failures"
fi
