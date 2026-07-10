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

CONTROLLER_GEN=controller-gen
HEADER=./hack/boilerplate.txt

if ! command -v "${CONTROLLER_GEN}" &>/dev/null; then
  echo "controller-gen not found. Installing..."
  go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.18.0
  export PATH="$(go env GOPATH)/bin:$PATH"
fi

echo "Generating deepcopy code for storage-foundation.deckhouse.io/v1alpha1 ..."

${CONTROLLER_GEN} object:headerFile="${HEADER}" paths=./api/v1alpha1

echo "Generating CRDs for storage-foundation.deckhouse.io/v1alpha1 ..."
${CONTROLLER_GEN} crd:crdVersions=v1 output:crd:dir=./crds/internal paths=./api/v1alpha1

# DataExport/DataImport CRDs are hand-curated in ./crds (they carry a non-standard `download`
# subresource and CEL immutability rules that controller-gen markers cannot express). Drop the
# auto-generated duplicates so the bundle does not ship two CRDs with the same metadata.name.
rm -f ./crds/internal/storage-foundation.deckhouse.io_dataexports.yaml \
      ./crds/internal/storage-foundation.deckhouse.io_dataimports.yaml

echo "Done."

