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

# check-consumer-crds.sh — guard against CRD drift between storage-foundation (the source of truth)
# and the deprecated consumer modules that ship byte-for-byte copies of its CRDs during the
# handover: snapshot-controller (the three snapshot.storage.k8s.io CRDs) and
# storage-volume-data-manager (dataexports/dataimports.storage-foundation.deckhouse.io).
#
# For each consumer it takes the consumer's LATEST git tag and diffs that tag's crds/<file> against
# storage-foundation's CURRENT crds/<file>. A mismatch means storage-foundation moved a CRD ahead
# of the consumer's released copy — cut a sync release of that consumer.
#
# Usage:
#   SNAPSHOT_CONTROLLER_REPO=/path/to/snapshot-controller \
#   SVDM_REPO=/path/to/storage-volume-data-manager \
#     hack/check-consumer-crds.sh
#
# In CI the two repos are shallow-clones created just before invoking this script; locally they are
# the workspace checkouts. A consumer with no matching tag yet, or an unset *_REPO, is skipped with
# a notice (the handover releases may not exist yet).
#
# NOTE: until a consumer has RELEASED its handover version (snapshot-controller v0.2.0, svdm v0.2.0)
# its latest tag still carries the pre-handover CRDs and this check will report a mismatch by
# design. Wire it as a BLOCKING gate only after those releases; before that, run it report-only.
set -euo pipefail

SF_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rc=0

# compare_at_tag <consumer-repo> <crds-file>
compare_at_tag() {
  local repo="$1" file="$2" tag="$3"
  local sf_file="${SF_ROOT}/crds/${file}"
  if [[ ! -f "${sf_file}" ]]; then
    echo "  ERROR: storage-foundation has no crds/${file}"
    return 1
  fi
  if ! git -C "${repo}" cat-file -e "${tag}:crds/${file}" 2>/dev/null; then
    echo "  MISS: ${file} absent at ${tag} (consumer not yet shipping this CRD)"
    return 1
  fi
  if diff -u <(git -C "${repo}" show "${tag}:crds/${file}") "${sf_file}" >/dev/null; then
    echo "  OK: ${file}"
  else
    echo "  MISMATCH: ${file} differs between storage-foundation (current) and ${tag}"
    return 1
  fi
}

check_consumer() {
  local name="$1" repo="$2"; shift 2
  local files=("$@")
  if [[ -z "${repo}" ]]; then
    echo "[${name}] skipped (repo path not provided)"
    return 0
  fi
  if [[ ! -d "${repo}/.git" ]]; then
    echo "[${name}] skipped (${repo} is not a git repo)"
    return 0
  fi
  local tag
  tag="$(git -C "${repo}" describe --tags --abbrev=0 2>/dev/null || true)"
  if [[ -z "${tag}" ]]; then
    echo "[${name}] skipped (no git tag found)"
    return 0
  fi
  echo "[${name}] comparing storage-foundation crds/ against ${name}@${tag}"
  local sub=0
  for f in "${files[@]}"; do
    compare_at_tag "${repo}" "${f}" "${tag}" || sub=1
  done
  return ${sub}
}

snapshot_crds=(
  snapshot.storage.k8s.io_volumesnapshots.yaml
  snapshot.storage.k8s.io_volumesnapshotcontents.yaml
  snapshot.storage.k8s.io_volumesnapshotclasses.yaml
  doc-ru-snapshot.storage.k8s.io_volumesnapshots.yaml
  doc-ru-snapshot.storage.k8s.io_volumesnapshotcontents.yaml
  doc-ru-snapshot.storage.k8s.io_volumesnapshotclasses.yaml
)
data_crds=(
  dataexports.yaml
  dataimports.yaml
  doc-ru-dataexports.yaml
  doc-ru-dataimports.yaml
)

check_consumer "snapshot-controller" "${SNAPSHOT_CONTROLLER_REPO:-}" "${snapshot_crds[@]}" || rc=1
check_consumer "storage-volume-data-manager" "${SVDM_REPO:-}" "${data_crds[@]}" || rc=1

if [[ ${rc} -ne 0 ]]; then
  echo
  echo "CRD drift detected: a consumer's latest tag no longer matches storage-foundation's crds/."
  echo "If storage-foundation intentionally changed a shared CRD, cut a sync release of the consumer."
fi
exit ${rc}
