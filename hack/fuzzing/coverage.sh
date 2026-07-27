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

# Replays the corpus sitting in cmd/testdata (seeds plus whatever `promote_corpus.sh`
# copied there) as a normal test run and reports how much of the module it reaches.

set -euo pipefail

MODULE_DIR="${1:?usage: coverage.sh <module_dir> <test_name>}" # Go module that was fuzzed
TEST_NAME="${2:?test name}" # Fuzz target whose corpus to replay

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${SCRIPT_DIR}/out"
TEST_DIR="${MODULE_DIR}/cmd"
CORPUS_DIR="${TEST_DIR}/testdata/fuzz/${TEST_NAME}"

mkdir -p "${OUT_DIR}"

if [ -d "${CORPUS_DIR}" ]; then
  echo "[coverage] Replaying $(find "${CORPUS_DIR}" -type f | wc -l | tr -d ' ') corpus files plus the seed corpus"
else
  echo "[coverage] No corpus in ${CORPUS_DIR}; measuring the seed corpus only"
  echo "[coverage] Run 'make fuzz' followed by 'make promote' to generate one"
fi

cd "${MODULE_DIR}"

# -coverpkg=./... is resolved against the module root, so the profile covers the whole
# image and not just the package holding the fuzz target.
go test \
  "${TEST_DIR}" \
  -run "${TEST_NAME}" \
  -coverpkg=./... \
  -coverprofile="${TEST_DIR}/coverage.txt" | tee "${TEST_DIR}/coverage_total.txt"

go tool cover -func="${TEST_DIR}/coverage.txt" > "${OUT_DIR}/coverage_func.txt"
go tool cover -html="${TEST_DIR}/coverage.txt" -o "${OUT_DIR}/coverage.html"
cp "${TEST_DIR}/coverage.txt" "${OUT_DIR}/coverage.txt"
cp "${TEST_DIR}/coverage_total.txt" "${OUT_DIR}/coverage_total.txt"

echo "[coverage] HTML report at ${OUT_DIR}/coverage.html"
