#!/bin/bash -e

source ./hack/build/config.sh
source ./cluster-up/hack/common.sh
source ./cluster-up/cluster/${KUBEVIRT_PROVIDER}/provider.sh

echo "Cleaning up ..."

OPERATOR_CR_MANIFEST=./_out/manifests/release/maroonedpods-cr.yaml
OPERATOR_MANIFEST=./_out/manifests/release/maroonedpods-operator.yaml
LABELS=("operator.maroonedpods.io" "maroonedpods.io" "prometheus.maroonedpods.io")
NAMESPACES=(default kube-system "${MAROONEDPODS_NAMESPACE}")



if [ -f "${OPERATOR_CR_MANIFEST}" ]; then
  echo "Cleaning CR object ..."
  _kubectl delete  --ignore-not-found  -f "${OPERATOR_CR_MANIFEST}" || true
  _kubectl wait  maroonedpods.io/${CR_NAME} --for=delete --timeout=30s || echo "this is fine"
fi


if [ "${MAROONEDPODS_CLEAN}" == "all" ] && [ -f "${OPERATOR_MANIFEST}" ]; then
	echo "Deleting operator ..."
  _kubectl delete --ignore-not-found -f "${OPERATOR_MANIFEST}"
fi

# Everything should be deleted by now, but just to be sure
for n in ${NAMESPACES[@]}; do
  for label in ${LABELS[@]}; do
    _kubectl -n ${n} delete deployment -l ${label} >/dev/null
    _kubectl -n ${n} delete services -l ${label} >/dev/null
    _kubectl -n ${n} delete secrets -l ${label} >/dev/null
    _kubectl -n ${n} delete configmaps -l ${label} >/dev/null
    _kubectl -n ${n} delete pods -l ${label} >/dev/null
    _kubectl -n ${n} delete rolebinding -l ${label} >/dev/null
    _kubectl -n ${n} delete roles -l ${label} >/dev/null
    _kubectl -n ${n} delete serviceaccounts -l ${label} >/dev/null
  done
done

for label in ${LABELS[@]}; do
    _kubectl delete pv -l ${label} >/dev/null
    _kubectl delete clusterrolebinding -l ${label} >/dev/null
    _kubectl delete clusterroles -l ${label} >/dev/null
    _kubectl delete customresourcedefinitions -l ${label} >/dev/null
done

if [ "${MAROONEDPODS_CLEAN}" == "all" ] && [ -n "$(_kubectl get ns | grep "maroonedpods ")" ]; then
    echo "Clean maroonedpods namespace"
    _kubectl delete ns MAROONEDPODS_NAMESPACE

    start_time=0
    sample=10
    timeout=120
    echo "Waiting for maroonedpods namespace to disappear ..."
    while [ -n "$(_kubectl get ns | grep "MAROONEDPODS_NAMESPACE ")" ]; do
        sleep $sample
        start_time=$((current_time + sample))
        if [[ $current_time -gt $timeout ]]; then
            exit 1
        fi
    done
fi
sleep 2
echo "Done"
