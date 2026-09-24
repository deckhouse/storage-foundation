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

# run from repository root

set -euo pipefail

HEADER=./hack/boilerplate.txt
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# controller-gen is pinned in hack/tools/go.mod and built into a temporary
# directory. It used to be taken from PATH, with v0.18.0 installed only when
# nothing was found there -- so the rendered manifests depended on whatever
# controller-gen the machine happened to have.
TOOLS_DIR="$(mktemp -d)"
trap 'rm -rf "${TOOLS_DIR}"' EXIT

echo "Building controller-gen (pinned in hack/tools/go.mod)..."
go -C "${ROOT_DIR}/hack/tools" build -o "${TOOLS_DIR}" \
    sigs.k8s.io/controller-tools/cmd/controller-gen

echo "Generating deepcopy code for storage-foundation.deckhouse.io/v1alpha1 ..."

"${TOOLS_DIR}/controller-gen" object:headerFile="${HEADER}" paths=./api/v1alpha1

# The CRD manifests are rendered and then enriched; that pair lives in its own
# script because the shared .crds_generated_check CI job runs it by name. It also
# owns the DataExport/DataImport exception that used to be an `rm -f` here.
echo "Generating CRDs for storage-foundation.deckhouse.io/v1alpha1 ..."
"${ROOT_DIR}/hack/generate_crds.sh"

echo "Done."

