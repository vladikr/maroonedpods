#!/bin/bash -e

maroonedpods=$1
maroonedpods="${maroonedpods##*/}"

echo maroonedpods

source ./hack/build/config.sh
source ./hack/build/common.sh
source ./cluster-up/hack/common.sh
source ./cluster-up/cluster/${KUBEVIRT_PROVIDER}/provider.sh

if [ "${KUBEVIRT_PROVIDER}" = "external" ]; then
   MAROONEDPODS_SYNC_PROVIDER="external"
else
   MAROONEDPODS_SYNC_PROVIDER="kubevirtci"
fi
source ./cluster-sync/${MAROONEDPODS_SYNC_PROVIDER}/provider.sh


MAROONEDPODS_NAMESPACE=${MAROONEDPODS_NAMESPACE:-maroonedpods}
MAROONEDPODS_INSTALL_TIMEOUT=${MAROONEDPODS_INSTALL_TIMEOUT:-120}
MAROONEDPODS_AVAILABLE_TIMEOUT=${MAROONEDPODS_AVAILABLE_TIMEOUT:-600}
MAROONEDPODS_PODS_UPDATE_TIMEOUT=${MAROONEDPODS_PODS_UPDATE_TIMEOUT:-480}
MAROONEDPODS_UPGRADE_RETRY_COUNT=${MAROONEDPODS_UPGRADE_RETRY_COUNT:-60}

# Set controller verbosity to 3 for functional tests.
export VERBOSITY=3

PULL_POLICY=${PULL_POLICY:-IfNotPresent}
# The default DOCKER_PREFIX is set to kubevirt and used for builds, however we don't use that for cluster-sync
# instead we use a local registry; so here we'll check for anything != "external"
# wel also confuse this by swapping the setting of the DOCKER_PREFIX variable around based on it's context, for
# build and push it's localhost, but for manifests, we sneak in a change to point a registry container on the
# kubernetes cluster.  So, we introduced this MANIFEST_REGISTRY variable specifically to deal with that and not
# have to refactor/rewrite any of the code that works currently.
MANIFEST_REGISTRY=$DOCKER_PREFIX

if [ "${KUBEVIRT_PROVIDER}" != "external" ]; then
  registry=${IMAGE_REGISTRY:-localhost:$(_port registry)}
  DOCKER_PREFIX=${registry}
  MANIFEST_REGISTRY="registry:5000"
fi

if [ "${KUBEVIRT_PROVIDER}" == "external" ]; then
  # No kubevirtci local registry, likely using something external
  if [[ $(${MAROONEDPODS_CRI} login --help | grep authfile) ]]; then
    registry_provider=$(echo "$DOCKER_PREFIX" | cut -d '/' -f 1)
    echo "Please log in to "${registry_provider}", bazel push expects external registry creds to be in ~/.docker/config.json"
    ${MAROONEDPODS_CRI} login --authfile "${HOME}/.docker/config.json" $registry_provider
  fi
fi

# Need to set the DOCKER_PREFIX appropriately in the call to `make docker push`, otherwise make will just pass in the default `kubevirt`

DOCKER_PREFIX=$MANIFEST_REGISTRY PULL_POLICY=$PULL_POLICY make manifests
DOCKER_PREFIX=$DOCKER_PREFIX make push


function check_structural_schema {
  for crd in "$@"; do
    status=$(_kubectl get crd $crd -o jsonpath={.status.conditions[?\(@.type==\"NonStructuralSchema\"\)].status})
    if [ "$status" == "True" ]; then
      echo "ERROR CRD $crd is not a structural schema!, please fix"
      _kubectl get crd $crd -o yaml
      exit 1
    fi
    echo "CRD $crd is a StructuralSchema"
  done
}

function wait_maroonedpods_available {
  echo "Waiting $MAROONEDPODS_AVAILABLE_TIMEOUT seconds for maroonedpods.io/${CR_NAME} to become available"
  if [ "$KUBEVIRT_PROVIDER" == "os-3.11.0-crio" ]; then
    echo "Openshift 3.11 provider"
    available=$(_kubectl get maroonedpods maroonedpods -o jsonpath={.status.conditions[0].status})
    wait_time=0
    while [[ $available != "True" ]] && [[ $wait_time -lt ${MAROONEDPODS_AVAILABLE_TIMEOUT} ]]; do
      wait_time=$((wait_time + 5))
      sleep 5
      sleep 5
      available=$(_kubectl get maroonedpods maroonedpods -o jsonpath={.status.conditions[0].status})
      fix_failed_sdn_pods
    done
  else
    
    _kubectl wait maroonedpods/${CR_NAME} -n ${MAROONEDPODS_NAMESPACE} --for=condition=Available --timeout=${MAROONEDPODS_AVAILABLE_TIMEOUT}s
  fi
}


OLD_MAROONEDPODS_VER_PODS="./_out/tests/old_maroonedpods_ver_pods"
NEW_MAROONEDPODS_VER_PODS="./_out/tests/new_maroonedpods_ver_pods"

mkdir -p ./_out/tests
rm -f $OLD_MAROONEDPODS_VER_PODS $NEW_MAROONEDPODS_VER_PODS

# Install MAROONEDPODS
install_maroonedpods

#wait maroonedpods crd is installed with timeout
wait_maroonedpods_crd_installed $MAROONEDPODS_INSTALL_TIMEOUT


_kubectl apply -f "./_out/manifests/release/maroonedpods-cr.yaml"
wait_maroonedpods_available



# Grab all the MAROONEDPODS crds so we can check if they are structural schemas
maroonedpods_crds=$(_kubectl get crd -l maroonedpods.io -o jsonpath={.items[*].metadata.name})
crds=($maroonedpods_crds)
operator_crds=$(_kubectl get crd -l operator.maroonedpods.io -o jsonpath={.items[*].metadata.name})
crds+=($operator_crds)
check_structural_schema "${crds[@]}"
