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

# Keep in sync with the golangci-lint version installed by the CI linter job
# (`.gitlab-ci.yml`, `.go_linter`): a local run has to read the same
# .golangci.yaml and enable the same set of linters as CI does. The config is
# written in the v2 schema, which only v2 binaries can read.
linter_version="v2.13.1"

# The upstream install.sh cannot install this version, so the download is done here.
# That script looks the tarball up in the release checksums with an unanchored grep, and
# since v2.12.2 the checksums also list "<tarball>.sbom.json" -- whose name contains the
# tarball's name. The lookup then yields TWO hashes, verification can never match, and the
# binary is silently not installed. What follows is that same verification with the lookup
# anchored to the whole file name, so the checksum still comes from the release and every
# platform keeps working.
section_start "install_linter" "Installing golangci-lint@$linter_version"
linter_os=$(uname -s | tr '[:upper:]' '[:lower:]')
linter_arch=$(uname -m)
case "${linter_arch}" in
    x86_64) linter_arch="amd64" ;;
    aarch64 | arm64) linter_arch="arm64" ;;
esac
linter_name="golangci-lint-${linter_version#v}-${linter_os}-${linter_arch}"
linter_base="https://github.com/golangci/golangci-lint/releases/download/${linter_version}"
curl -sSfL -o "${linter_name}.tar.gz" "${linter_base}/${linter_name}.tar.gz"
curl -sSfL -o golangci-lint-checksums.txt "${linter_base}/golangci-lint-${linter_version#v}-checksums.txt"
linter_want=$(awk -v f="${linter_name}.tar.gz" '$2 == f { print $1 }' golangci-lint-checksums.txt)
if [ -z "${linter_want}" ]; then
    echo -e "\e[31mno checksum for ${linter_name}.tar.gz in the release checksums\e[0m"
    exit 1
fi
echo "${linter_want}  ${linter_name}.tar.gz" | sha256sum -c - || exit 1
tar -xzf "${linter_name}.tar.gz" --strip-components=1 "${linter_name}/golangci-lint"
rm -f "${linter_name}.tar.gz" golangci-lint-checksums.txt
section_end "install_linter"

basedir=$(pwd)
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
            ../../golangci-lint run ${extra_args} --fix --color=always --allow-parallel-runners --build-tags $edition
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
        git checkout -f
        failed='true'
    else
        echo -e "\e[32mLinter doesn't have changes requested for $run_for\e[0m"
    fi
}

if [ -n "${CI_MERGE_REQUEST_DIFF_BASE_SHA}" ]; then
    run_linters "modified files" "--new-from-merge-base=${CI_MERGE_REQUEST_DIFF_BASE_SHA}"
fi

run_linters "all files"

rm golangci-lint

if [ $failed == 'true' ]; then
    exit 1
fi
