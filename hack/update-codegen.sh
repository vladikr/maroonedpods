#!/usr/bin/env bash

# Copyright 2017 The Kubernetes Authors.
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

set -o errexit
set -o nounset
set -o pipefail
export GO111MODULE=on

export SCRIPT_ROOT="$(cd "$(dirname $0)/../" && pwd -P)"
CODEGEN_PKG=${CODEGEN_PKG:-$(
    cd ${SCRIPT_ROOT}
    ls -d -1 ./vendor/k8s.io/code-generator 2>/dev/null || echo ../code-generator
)}

find "${SCRIPT_ROOT}/pkg/" -name "*generated*.go" -exec rm {} -f \;
find "${SCRIPT_ROOT}/staging/src/maroonedpods.io/api/" -name "*generated*.go" -exec rm {} -f \;
rm -rf "${SCRIPT_ROOT}/pkg/generated"

# generate the code with:
# --output-base    because this script should also be able to run inside the vendor dir of
#                  k8s.io/kubernetes. The output-base is needed for the generators to output into the vendor dir
#                  instead of the $GOPATH directly. For normal projects this can be dropped.
/bin/bash ${CODEGEN_PKG}/generate-groups.sh  "deepcopy,client,informer,lister" \
  maroonedpods.io/maroonedpods/pkg/generated/maroonedpods \
  maroonedpods.io/maroonedpods/staging/src/maroonedpods.io/api/pkg/apis  \
    "core:v1alpha1 " \
    --go-header-file ${SCRIPT_ROOT}/hack/custom-boilerplate.go.txt


/bin/bash ${CODEGEN_PKG}/generate-groups.sh  "client" \
  maroonedpods.io/maroonedpods/pkg/generated/kubevirt \
  kubevirt.io/api  \
    "core:v1 " \
    --go-header-file ${SCRIPT_ROOT}/hack/custom-boilerplate.go.txt

echo "************* running controller-gen to generate schema yaml ********************"
(
    mkdir -p "${SCRIPT_ROOT}/_out/manifests/schema"
    find "${SCRIPT_ROOT}/_out/manifests/schema/" -type f -exec rm {} -f \;
    cd ./staging/src/maroonedpods.io/api
    echo pwd
    controller-gen crd:crdVersions=v1 output:dir=${SCRIPT_ROOT}/_out/manifests/schema paths=./pkg/apis/core/...
)

(cd "${SCRIPT_ROOT}/tools/crd-generator/" && go build -o "${SCRIPT_ROOT}/bin/crd-generator" -buildvcs=false ./...)
${SCRIPT_ROOT}/bin/crd-generator --crdDir=${SCRIPT_ROOT}/_out/manifests/schema/ --outputDir=${SCRIPT_ROOT}/pkg/maroonedpods-operator/resources/
