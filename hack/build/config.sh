#Copyright 2023 The MaroonedPods Authors.
#
#Licensed under the Apache License, Version 2.0 (the "License");
#you may not use this file except in compliance with the License.
#You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
#Unless required by applicable law or agreed to in writing, software
#distributed under the License is distributed on an "AS IS" BASIS,
#WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#See the License for the specific language governing permissions and
#limitations under the License.

OPERATOR_IMAGE_NAME=${OPERATOR_IMAGE_NAME:-maroonedpods-operator}
CONTROLLER_IMAGE_NAME=${CONTROLLER_IMAGE_NAME:-maroonedpods-controller}
MAROONEDPODS_SERVER_IMAGE_NAME=${MAROONEDPODS_SERVER_IMAGE_NAME:-maroonedpods-server}
DOCKER_PREFIX=${DOCKER_PREFIX:-"quay.io/vladikr"}
DOCKER_TAG=${DOCKER_TAG:-latest}
VERBOSITY=${VERBOSITY:-1}
PULL_POLICY=${PULL_POLICY:-Always}
MAROONEDPODS_NAMESPACE=${MAROONEDPODS_NAMESPACE:-maroonedpods}
CR_NAME=${CR_NAME:-maroonedpods}

# update this whenever new builder tag is created
BUILDER_IMAGE=${BUILDER_IMAGE:-quay.io/vladikr/maroonedpods-bazel-builder:2401250613-e6da931}

function parseTestOpts() {
    pkgs=""
    test_args=""
    while [[ $# -gt 0 ]] && [[ $1 != "" ]]; do
        case "${1}" in
        --test-args=*)
            test_args="${1#*=}"
            shift 1
            ;;
        ./*...)
            pkgs="${pkgs} ${1}"
            shift 1
            ;;
        *)
            echo "ABORT: Unrecognized option \"$1\""
            exit 1
            ;;
        esac
    done
}
