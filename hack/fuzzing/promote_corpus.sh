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

# Copies the corpus the fuzzer generated in .fuzzcache into cmd/testdata/fuzz, where a
# plain `go test` replays it. This is what turns a fuzzing run into a reusable artifact.

set -euo pipefail

MODULE_DIR="${1:?usage: promote_corpus.sh <module_dir> <targets>}" # Go module that was fuzzed
TARGETS="${2:?targets list}" # Space-separated list of fuzz targets to promote

TEST_DIR="${MODULE_DIR}/cmd"
CACHE_DIR="${TEST_DIR}/.fuzzcache"

for tgt in ${TARGETS}; do
  src="${CACHE_DIR}/${tgt}"
  dst="${TEST_DIR}/testdata/fuzz/${tgt}"

  if [ ! -d "${src}" ]; then
    echo "[promote] ${tgt}: no corpus in ${src}, nothing to promote"
    continue
  fi

  mkdir -p "${dst}"
  cp -R "${src}/." "${dst}/"
  echo "[promote] ${tgt}: $(find "${dst}" -type f | wc -l | tr -d ' ') corpus files in ${dst}"
done
