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

# Packs the fuzzing log, the coverage report and the corpus into a single tarball, with a
# summary recording which commit and toolchain produced it.

set -euo pipefail

MODULE_DIR="${1:?usage: archive.sh <module_dir>}" # Go module that was fuzzed

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${SCRIPT_DIR}/out"
TESTDATA_SRC="${MODULE_DIR}/cmd/testdata"

mkdir -p "${OUT_DIR}"

if [ -d "${TESTDATA_SRC}" ]; then
  echo "[archive] Copying ${TESTDATA_SRC} -> ${OUT_DIR}/testdata"
  rm -rf "${OUT_DIR}/testdata"
  cp -R "${TESTDATA_SRC}" "${OUT_DIR}/testdata"
else
  echo "[archive] No testdata at ${TESTDATA_SRC}, skipping corpus copy"
fi

{
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "go: $(go version 2>/dev/null || echo 'go not found')"
  echo "commit: $(git -C "${SCRIPT_DIR}" rev-parse HEAD 2>/dev/null || echo 'unknown')"
  echo "module: ${MODULE_DIR}"
  echo "fuzz exit status per target:"
  sed 's/^/  /' "${OUT_DIR}/fuzz_status.txt" 2>/dev/null || echo "  unknown"
} > "${OUT_DIR}/summary.txt"

echo "[archive] Wrote ${OUT_DIR}/summary.txt"

ARCHIVE_NAME="fuzz_report-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"

# Written next to out/ rather than inside it, so the archive never contains itself.
tar -czf "${SCRIPT_DIR}/${ARCHIVE_NAME}" -C "${SCRIPT_DIR}" out

echo "[archive] Archive created: ${SCRIPT_DIR}/${ARCHIVE_NAME}"
