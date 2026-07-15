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

# check-migration-hook-parity.sh — guard the CONTRACT shared by the two copies of the
# 025-migrate-legacy-crds hook: storage-foundation's (this repo) and
# storage-volume-data-manager's (svdm). Both hooks migrate DataExport/DataImport from the legacy
# storage.deckhouse.io group to storage-foundation.deckhouse.io; the exact mapping tables and
# spec-transform field operations MUST stay identical, or a cluster migrated by one module ends up
# with a different result than one migrated by the other.
#
# The two files are NOT byte-for-byte copies (different package import path, and storage-foundation
# factored the pure transform into mapLegacySpec). So instead of diffing whole files this script
# extracts and diffs only the three parts of the shared contract, all of which are kept verbatim:
#   1. the exportKindToGroup table (legacy DataExport targetRef.kind -> new API group);
#   2. the failedReasons set in isActiveLegacyResource (which legacy CRs are considered finished);
#   3. every unstructured.* field operation (the spec transforms + the object-build field ops),
#      which encode the JSON paths and literal values of the migration ("CreatePVC", "targetRef",
#      "pvcTemplate", "group", ...).
#
# It compares storage-foundation's CURRENT copy against svdm's copy at svdm's LATEST git tag.
#
# Usage:
#   SVDM_REPO=/path/to/storage-volume-data-manager hack/check-migration-hook-parity.sh
#
# An unset SVDM_REPO, a non-git path, a missing tag, or the file being absent at that tag are all
# skipped with a notice (exit 0) — the same lenient contract as hack/check-consumer-crds.sh.
#
# Both 025 hooks are transitional/expiring. Run this report-only until both modules have released
# their handover version; wire it as a BLOCKING CI gate (like the intended end-state of
# check-consumer-crds.sh) only afterwards. A real MISMATCH exits non-zero so it can gate once wired.
set -euo pipefail

SF_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK_REL="hooks/go/025-migrate-legacy-crds/migrate-legacy-crds.go"
SF_HOOK="${SF_ROOT}/${HOOK_REL}"

SVDM_REPO="${SVDM_REPO:-}"

if [[ ! -f "${SF_HOOK}" ]]; then
  echo "[migration-hook-parity] ERROR: storage-foundation has no ${HOOK_REL}"
  exit 1
fi
if [[ -z "${SVDM_REPO}" ]]; then
  echo "[migration-hook-parity] skipped (SVDM_REPO not set)"
  exit 0
fi
if [[ ! -d "${SVDM_REPO}/.git" ]]; then
  echo "[migration-hook-parity] skipped (${SVDM_REPO} is not a git repo)"
  exit 0
fi

tag="$(git -C "${SVDM_REPO}" describe --tags --abbrev=0 2>/dev/null || true)"
if [[ -z "${tag}" ]]; then
  echo "[migration-hook-parity] skipped (no git tag found in ${SVDM_REPO})"
  exit 0
fi
if ! git -C "${SVDM_REPO}" cat-file -e "${tag}:${HOOK_REL}" 2>/dev/null; then
  echo "[migration-hook-parity] skipped (${HOOK_REL} absent at ${tag})"
  exit 0
fi

echo "[migration-hook-parity] comparing storage-foundation (current) against storage-volume-data-manager@${tag}"

svdm_hook() { git -C "${SVDM_REPO}" show "${tag}:${HOOK_REL}"; }

# extract_map <map-anchor-regex> — reads a Go source file on stdin and emits the sorted, trimmed
# entries of the map literal opened by the anchor line, up to its closing brace.
extract_map() {
  awk -v anchor="$1" '
    $0 ~ anchor { inblk = 1; next }
    inblk {
      line = $0
      gsub(/^[ \t]+|[ \t]+$/, "", line)
      if (line == "}") { inblk = 0; next }
      if (line == "") next
      print line
    }
  ' | sort
}

# extract_ops — reads a Go source file on stdin and emits the sorted set of unstructured.* field
# operations (call + literal arguments), normalizing away the surrounding statement structure.
extract_ops() {
  grep -oE 'unstructured\.[A-Za-z]+\([^)]*\)' | sort -u
}

rc=0
compare() {
  local label="$1" sf_out="$2" svdm_out="$3"
  # An empty extraction means an anchor stopped matching (e.g. the map was renamed): treat it as a
  # failure instead of letting empty==empty pass vacuously.
  if [[ -z "${sf_out}" || -z "${svdm_out}" ]]; then
    echo "  ERROR: ${label} — extractor produced no output (anchor broken / block renamed?)"
    rc=1
    return
  fi
  if diff <(printf '%s\n' "${sf_out}") <(printf '%s\n' "${svdm_out}") >/dev/null; then
    echo "  OK: ${label}"
  else
    echo "  MISMATCH: ${label}"
    diff <(printf '%s\n' "${svdm_out}") <(printf '%s\n' "${sf_out}") | sed 's/^/    /' || true
    rc=1
  fi
}

svdm_src="$(svdm_hook)"

# Anchors match only up to `= map` / `:= map`: the trailing `[string]...` would be read as an awk
# regex where `[...]` is a character class and would never match the literal brackets in the source.
compare "exportKindToGroup table" \
  "$(extract_map 'exportKindToGroup = map' < "${SF_HOOK}")" \
  "$(printf '%s\n' "${svdm_src}" | extract_map 'exportKindToGroup = map')"

compare "isActiveLegacyResource failedReasons" \
  "$(extract_map 'failedReasons := map' < "${SF_HOOK}")" \
  "$(printf '%s\n' "${svdm_src}" | extract_map 'failedReasons := map')"

compare "spec-transform field operations" \
  "$(extract_ops < "${SF_HOOK}")" \
  "$(printf '%s\n' "${svdm_src}" | extract_ops)"

if [[ ${rc} -ne 0 ]]; then
  echo
  echo "Migration-hook contract drift detected between storage-foundation and storage-volume-data-manager."
  echo "The two 025-migrate-legacy-crds hooks must map legacy resources identically. Reconcile the"
  echo "mapping table / transforms in BOTH repos (diff shown: '-' svdm@${tag}, '+' storage-foundation)."
fi
exit ${rc}
