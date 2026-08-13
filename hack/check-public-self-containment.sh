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

# check-public-self-containment.sh — guard that the texts of this repository can be read on their own.
#
# THE RULE LIVES IN CLAUDE.md, section "Comments and descriptions are self-contained", and is
# deliberately NOT restated here: two copies of a norm drift apart silently, and the copy nobody
# runs is the one that goes stale. This header is only about the check — what it matches, what it
# skips, what it cannot see. Read the rule there; read the limits below before trusting a green run.
#
# WHAT IT LOOKS FOR
#   Short labels that carry meaning only for someone holding the other document — ids of components,
#   phases and rollout waves, references to a numbered section, ids of process steps and of numbered
#   or lettered alternatives. The exact expressions, each with the shape it stands for, are the
#   check_pattern calls at the bottom of this file; that list is the specification, not this prose.
#
#   Matching is BY WORD, never by substring. Two-character labels (a letter followed by a digit)
#   appear by accident inside machine-generated checksum blobs — go.sum is full of them — and a
#   substring search would bury every real finding under that noise until someone switched the whole
#   check off as useless.
#
# WHAT IT SKIPS, AND WHY
#   * Vendored upstream copies: a `vendor` directory at the repository root or at any depth (today
#     that is crds/vendor/). This is third-party text the repository does not author.
#   * Exactly one patch file, named by its FULL PATH below. A patch carries the exact bytes applied
#     to a third-party checkout, so rewriting text inside an added line changes what the fork builds
#     and can only be verified by building it; that one file keeps such a label for now. The
#     exclusion is a full path and deliberately NOT a `*.patch` glob — a label leaking into any other
#     patch file must still be caught.
#
# WHAT IT CANNOT CATCH — the limit of this check, know it before trusting a green run
#   * A pointer written out in prose ("see the design document, section six"): nothing mechanical to
#     match, so the check stays silent.
#   * The name of a repository, host or wiki the reader cannot reach.
#   * A retelling of the content of a document nobody outside can open — the worst case, because the
#     text looks self-contained while its correctness rests on something invisible.
#   Those three need a human reviewer. This script catches only the mechanical, greppable form.
#   It also looks at TRACKED files only, and files that grep treats as binary are not searched.
#
# HOW TO FIX A FINDING
#   Say what the label stood for. Do not widen the expressions below, and do not add an exclusion to
#   make a real finding go away — that turns the guard into decoration.
#
# Usage:
#   hack/check-public-self-containment.sh
#
# No arguments, no environment, no network. Exit code: 0 clean, 1 findings, 2 the check could not
# run (and therefore proves nothing).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}" || exit 2

SKIP_PATHS=(
  ':(exclude)vendor/**'
  ':(exclude)**/vendor/**'
  ':(exclude)images/snapshot-controller/patches/003-volumesnapshot-dataimport-fork.patch'
)

mapfile -d '' -t tracked_all < <(git ls-files -z)
mapfile -d '' -t candidates < <(git ls-files -z -- "${SKIP_PATHS[@]}")

# A file can be listed in the index and gone from the working tree; count those instead of letting
# grep fail on them.
files=()
absent=0
for f in "${candidates[@]}"; do
  if [[ -f "${f}" ]]; then
    files+=("${f}")
  else
    absent=$((absent + 1))
  fi
done

# Sanity check on the list itself: a typo in the path expressions above, or a run from the wrong
# directory, would leave the list empty or root-only, and every pattern below would then report
# "clean" while having read nothing. So demand proof that the list is real: the module manifest at
# the repository root must be in it (the working directory is right) and so must at least one file
# inside a subdirectory (subdirectories were not excluded away).
seen_anchor=no
seen_nested=no
for f in "${files[@]}"; do
  if [[ "${f}" == "module.yaml" ]]; then
    seen_anchor=yes
  fi
  if [[ "${f}" == */* ]]; then
    seen_nested=yes
  fi
  if [[ "${seen_anchor}" == yes && "${seen_nested}" == yes ]]; then
    break
  fi
done
if [[ ${#files[@]} -eq 0 || "${seen_anchor}" != yes || "${seen_nested}" != yes ]]; then
  echo "ERROR: the file list came out wrong: ${#files[@]} file(s)," \
    "module manifest in the list: ${seen_anchor}, files from subdirectories: ${seen_nested}." >&2
  echo "       Almost nothing was searched, so a green result would mean nothing." \
    "Fix the path expressions or run the script from within the repository." >&2
  exit 2
fi

# The section sign is written as its UTF-8 bytes so that this file does not match its own pattern.
SECTION_SIGN="$(printf '\302\247')"

findings_total=0

# check_pattern <extended-regex> <what the label stands for>
#
# One grep per pattern over the whole file list. At this repository's size — hundreds of files, a few
# tens of kilobytes of arguments — that is far from the argument-list limit; if the repository ever
# grows by an order of magnitude, feed the list through xargs instead.
check_pattern() {
  local regex="$1" label="$2" hits count
  hits="$(grep -I -n -E -e "${regex}" -- "${files[@]}" || true)"
  if [[ -z "${hits}" ]]; then
    printf '  ok     %-38s %s\n' "${label}" "${regex}"
    return 0
  fi
  count="$(printf '%s\n' "${hits}" | wc -l)"
  count="${count// /}"
  findings_total=$((findings_total + count))
  printf '  FOUND  %-38s %s  (%s)\n' "${label}" "${regex}" "${count}"
  printf '%s\n' "${hits}" | sed 's/^/           /'
}

echo "checking that the texts of this repository do not point at material a reader cannot open"
echo

# The word boundaries are the point of every expression here, not decoration: without them the first
# two would match the middle of any checksum blob. The wave expression deliberately has no closing
# boundary, so that a label carrying a letter suffix after the digit is caught as well.
check_pattern '\bC[0-9]+\b'      'component id'
check_pattern '\bD[0-9]+\b'      'phase id'
check_pattern '\b[Ww]ave[0-9]'   'rollout wave id'
check_pattern "${SECTION_SIGN}"  'reference to a numbered section'
check_pattern '\bBlock [0-9]'    'process step id'
check_pattern '\b[Dd]ecision #'  'numbered decision reference'
check_pattern '\bOption [A-C]\b' 'lettered alternative'

echo
echo "searched ${#files[@]} of ${#tracked_all[@]} tracked files:" \
  "$(( ${#tracked_all[@]} - ${#candidates[@]} )) skipped by the rules above," \
  "${absent} listed but absent from the working tree"
echo "found ${findings_total} occurrence(s)"

if [[ ${findings_total} -gt 0 ]]; then
  cat >&2 <<'EOF'

Every occurrence above is a short label that means something only to someone holding a document this
repository does not contain. Replace each one with what it stood for: state the rule or the invariant
in place, in full words, and where the invariant must not drift, name the test in this repository
that keeps it honest.

Do not widen the expressions and do not add an exclusion to silence a real finding.
EOF
  exit 1
fi

echo "OK"
