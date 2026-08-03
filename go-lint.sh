#!/bin/bash

# Copyright 2025 Flant JSC
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

unique_index=0
section_start() {
    local section_title="${1}"
    local section_description="${2}"
    
    if [ "$GITLAB_CI" == "true" ]; then
        unique_index=$((unique_index + 1))
        echo -e "section_start:`date +%s`:${section_title}_${unique_index}[collapsed=true]\r\e[0K${section_description}"
    else
        echo "$section_description"
    fi
}

section_end() {
    local section_title="${1}"
    if [ "$GITLAB_CI" == "true" ]; then
        echo -e "section_end:`date +%s`:${section_title}_${unique_index}\r\e[0K"
    fi
}

basedir=$(pwd)

# Version and install method are those of CI (deckhouse/modules-actions/go_linter). A mismatch here
# does not give a different report, it gives no report at all: the configuration file is rejected
# outright by the other major version of golangci-lint.
linter_version="v2.9.0"

# The linter type-checks the sources it lints, so it must be built with a toolchain no older than the
# newest go directive among the modules — otherwise it panics on files it cannot parse. The `go` pin
# in .golangci.yaml does not help: it only silences the version check that would have reported this
# before the panic. Taking the version from the modules keeps the two in step without a second place
# to bump.
default_toolchain="go$(grep -h '^go ' $(find images -type f -name go.mod) | awk '{print $2}' | sort -V | tail -1)"
linter_toolchain="${GOLANGCI_LINT_GOTOOLCHAIN:-$default_toolchain}"

# Keyed by both versions, so a bump of either builds a new binary instead of reusing a stale one.
# Outside the repository on purpose: the previous location was the working tree, one .gitignore entry
# away from being committed, and the run had to end by deleting it.
linter_cache_root="${GOLANGCI_LINT_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/storage-foundation/golangci-lint}"
linter_bin_dir="$linter_cache_root/$linter_toolchain/$linter_version"
linter_bin="$linter_bin_dir/golangci-lint"

section_start "install_linter" "Installing golangci-lint@$linter_version with $linter_toolchain"
if [ ! -x "$linter_bin" ]; then
    mkdir -p "$linter_bin_dir"
    if ! GOBIN="$linter_bin_dir" GOTOOLCHAIN="$linter_toolchain" go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$linter_version"; then
        section_end "install_linter"
        exit 1
    fi
fi
section_end "install_linter"
failed='false'

run_linters() {
    local run_for="${1}"
    local extra_args="${2}"
    for i in $(find images -type f -name go.mod);do
        dir=$(echo $i | sed 's/go.mod$//')
        cd $basedir/$dir
        # check all editions
        for edition in $GO_BUILD_TAGS ;do
            section_start "run_lint" "Running linter in $dir (edition: $edition) for $run_for"
            "$linter_bin" run ${extra_args} --fix --color=always --allow-parallel-runners --build-tags $edition
            local linter_status=$?
            section_end "run_lint"
            if [ $linter_status -ne 0 ]; then
                echo -e "\e[31mLinter FAILED in $dir (edition: $edition) for $run_for\e[0m"
                failed='true'
            else
                echo -e "\e[32mLinter PASSED in $dir (edition: $edition) for $run_for\e[0m"
            fi
        done

        cd - > /dev/null
    done


    if [[ -n "$(git status --porcelain --untracked-files=no)" ]]; then
        echo -e "\e[31mLinter requests changes for $run_for\e[0m"
        section_start "print_patch" "To apply these changes run"
        echo "git apply - <<EOF
$(git diff)
EOF"
        section_end "print_patch" 
        # The edits are left in the working copy: --fix ran before this check, so a reset here cannot
        # tell an auto-fix from work in progress and would discard both. Review the diff above and
        # commit or revert it.
        failed='true'
    else
        echo -e "\e[32mLinter doesn't have changes requested for $run_for\e[0m"
    fi
}

if [ -n "${CI_MERGE_REQUEST_DIFF_BASE_SHA}" ]; then
    run_linters "modified files" "--new-from-merge-base=${CI_MERGE_REQUEST_DIFF_BASE_SHA}"
fi

run_linters "all files"

if [ $failed == 'true' ]; then
    exit 1
fi
