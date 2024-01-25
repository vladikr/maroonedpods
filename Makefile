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

.PHONY: manifests \
		cluster-up cluster-down cluster-sync \
		test test-functional test-unit test-lint \
		publish \
		maroonedpods_controller \
		maroonedpods_server \
		maroonedpods_operator \
		fmt \
		goveralls \
		release-description \
		bazel-build-images push-images \
		fossa
all: build

build:  maroonedpods_controller maroonedpods_server maroonedpods_operator

DOCKER?=1
ifeq (${DOCKER}, 1)
	# use entrypoint.sh (default) as your entrypoint into the container
	DO=./hack/build/in-docker.sh
	# use entrypoint-bazel.sh as your entrypoint into the container.
	DO_BAZ=./hack/build/bazel-docker.sh
else
	DO=eval
	DO_BAZ=eval
endif

ifeq ($(origin KUBEVIRT_RELEASE), undefined)
	KUBEVIRT_RELEASE="latest_nightly"
endif

all: manifests build-images

manifests:
	${DO_BAZ} "DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} VERBOSITY=${VERBOSITY} PULL_POLICY=${PULL_POLICY} CR_NAME=${CR_NAME} MAROONEDPODS_NAMESPACE=${MAROONEDPODS_NAMESPACE} ./hack/build/build-manifests.sh"

builder-push:
	./hack/build/bazel-build-builder.sh

generate:
	${DO_BAZ} "./hack/update-codegen.sh"

generate-verify: generate
	./hack/verify-generate.sh
	./hack/check-for-binaries.sh

cluster-up:
	eval "KUBEVIRT_RELEASE=${KUBEVIRT_RELEASE} ./cluster-up/up.sh"

cluster-down:
	./cluster-up/down.sh

push-images:
	eval "DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG}  ./hack/build/build-docker.sh push"

build-images:
	eval "DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG}  ./hack/build/build-docker.sh"

push: build-images push-images

cluster-clean-maroonedpods:
	./cluster-sync/clean.sh

cluster-sync: cluster-clean-maroonedpods
	./cluster-sync/sync.sh MAROONEDPODS_AVAILABLE_TIMEOUT=${MAROONEDPODS_AVAILABLE_TIMEOUT} DOCKER_PREFIX=${DOCKER_PREFIX} DOCKER_TAG=${DOCKER_TAG} PULL_POLICY=${PULL_POLICY} MAROONEDPODS_NAMESPACE=${MAROONEDPODS_NAMESPACE}

test: WHAT = ./pkg/... ./cmd/...
test: bootstrap-ginkgo
	${DO_BAZ} "ACK_GINKGO_DEPRECATIONS=${ACK_GINKGO_DEPRECATIONS} ./hack/build/run-unit-tests.sh ${WHAT}"

build-functest:
	${DO_BAZ} ./hack/build/build-functest.sh

functest:  WHAT = ./tests/...
functest: build-functest
	./hack/build/run-functional-tests.sh ${WHAT} "${TEST_ARGS}"

bootstrap-ginkgo:
	${DO_BAZ} ./hack/build/bootstrap-ginkgo.sh

maroonedpods_controller:
	go build -o maroonedpods_controller -v cmd/maroonedpods-controller/*.go
	chmod 777 maroonedpods_controller

maroonedpods_operator:
	go build -o maroonedpods_operator -v cmd/maroonedpods-operator/*.go
	chmod 777 maroonedpods_operator

maroonedpods_server:
	go build -o maroonedpods_server -v cmd/maroonedpods-server/*.go
	chmod 777 maroonedpods_server

csv-generator:
	go build -o bin/csv-generator -v tools/csv-generator/csv-generator.go
	chmod 777 bin/csv-generator

clean:
	rm ./maroonedpods_controller ./maroonedpods_operator ./maroonedpods_server -f


fmt:
	go fmt .

run: build
	sudo ./maroonedpods_controller
