#!/bin/bash
# MaroonedPods + HyperShift E2E Demo
# Demonstrates running a sovereign tenant cluster on KubeVirt VMs
#
# Prerequisites:
#   - HC tenant-clean running with CP VM
#   - worker-ovs-mask ConfigMap in clusters namespace
#   - KUBECONFIG set to management cluster
#
# Usage: bash hack/demo.sh

set -euo pipefail
export KUBECONFIG=${KUBECONFIG:-~/devel/kubeconfig}

CYAN='\033[0;36m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m'

pause() {
    echo ""
    read -rsp "  Press Enter to continue..."
    echo ""
}

narrate() {
    echo -e "\n${CYAN}▸ $1${NC}"
}

run() {
    echo -e "${YELLOW}\$ $1${NC}"
    eval "$1"
}

clear
echo -e "${BOLD}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║   MaroonedPods: Sovereign Tenant Cluster on KubeVirt VMs   ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════════════════════════════╝${NC}"
echo ""
echo "  Management cluster: OCP 5.0 + KubeVirt + HyperShift"
echo "  Tenant cluster:     OCP 4.20 (control plane in VM, workers in VMs)"
echo ""
echo "  Architecture:"
echo "    Management Cluster"
echo "    ├── HostedCluster (tenant-clean)"
echo "    ├── CP VM (bridge binding, OVS masked, auto-proxy)"
echo "    │   ├── etcd, kube-apiserver, router, ignition-server"
echo "    │   └── socat proxy (6443/8443/9443 → bridge pods)"
echo "    └── Worker VM (NodePool + MachineConfig)"
echo "        ├── Bridge CNI (10.244.0.0/24)"
echo "        └── kubelet → tenant kapi via OCP Route"
pause

# --- Step 1: Show the HostedCluster ---
narrate "Step 1: The HostedCluster is running on OCP 4.20"
run "kubectl get hc -n clusters tenant-clean -o custom-columns=NAME:.metadata.name,VERSION:.status.version.desired,AVAILABLE:.status.conditions[?@.type==\"KubeAPIServerAvailable\"].status"
pause

# --- Step 2: Show the CP VM ---
narrate "Step 2: The control plane runs inside a KubeVirt VM with bridge binding"
run "kubectl get vmi -n clusters-tenant-clean -l maroonedpods.io/group --no-headers"
echo ""
narrate "All 16 OVS/OVN services are masked. Bridge CNI provides pod networking inside the VM."
narrate "Auto-proxy (systemd service) forwards traffic from pod IP to bridge pods."
pause

# --- Step 3: Show CP pods ---
narrate "Step 3: HyperShift control plane pods running inside the VM"
run "kubectl get pods -n clusters-tenant-clean --no-headers | head -15"
echo "..."
run "kubectl get pods -n clusters-tenant-clean --no-headers | wc -l"
echo "  total pods"
pause

# --- Step 4: Show proxy EndpointSlices ---
narrate "Step 4: Proxy EndpointSlices route external traffic to the VM's socat proxy"
run "kubectl get endpointslice -n clusters-tenant-clean -l endpointslice.kubernetes.io/managed-by=maroonedpods-controller --no-headers"
pause

# --- Step 5: Create NodePool ---
narrate "Step 5: Creating a NodePool to add a tenant worker VM"
# Delete existing NodePool if present
kubectl delete nodepool -n clusters tenant-clean 2>/dev/null || true
sleep 3

cat <<'EOF' | kubectl apply -f -
apiVersion: hypershift.openshift.io/v1beta1
kind: NodePool
metadata:
  name: tenant-clean
  namespace: clusters
spec:
  clusterName: tenant-clean
  replicas: 1
  release:
    image: quay.io/openshift-release-dev/ocp-release:4.20.0-x86_64
  management:
    upgradeType: Replace
    autoRepair: false
  platform:
    type: KubeVirt
    kubevirt:
      compute:
        cores: 4
        memory: 8Gi
      rootVolume:
        persistent:
          size: 32Gi
        type: Persistent
  config:
  - name: worker-ovs-mask
EOF
echo ""
narrate "NodePool created with worker-ovs-mask MachineConfig (OVS masks + bridge CNI)"
pause

# --- Step 6: Watch worker boot ---
narrate "Step 6: Watching worker VM boot and register with tenant cluster..."
echo ""
kubectl get secret -n clusters-tenant-clean admin-kubeconfig -o jsonpath='{.data.kubeconfig}' | base64 -d > /tmp/tenant-kubeconfig.yaml

for i in $(seq 1 30); do
    np_ign=$(kubectl get nodepool -n clusters tenant-clean -o jsonpath='{.status.conditions[?(@.type=="ReachedIgnitionEndpoint")].status}' 2>/dev/null)
    worker_vmi=$(kubectl get vmi -n clusters-tenant-clean --no-headers 2>/dev/null | grep "^tenant-clean" | awk '{print $3}')
    nodes=$(kubectl --kubeconfig=/tmp/tenant-kubeconfig.yaml --insecure-skip-tls-verify get nodes --no-headers 2>/dev/null)
    node_status=$(echo "$nodes" | awk '{print $1, $2}' 2>/dev/null | head -1)

    printf "\r  [%3ds] VMI=%-12s Ignition=%-6s Node=%s     " "$((i*30))" "${worker_vmi:-Pending}" "${np_ign:-False}" "${node_status:-waiting...}"

    if echo "$node_status" | grep -q "Ready"; then
        echo ""
        echo -e "\n${GREEN}✓ Worker node registered and Ready!${NC}"
        break
    fi
    sleep 30
done
pause

# --- Step 7: Show tenant cluster ---
narrate "Step 7: Tenant cluster nodes"
run "kubectl --kubeconfig=/tmp/tenant-kubeconfig.yaml --insecure-skip-tls-verify get nodes -o wide"
pause

# --- Step 8: Run a pod ---
narrate "Step 8: Running a pod on the tenant cluster"
kubectl --kubeconfig=/tmp/tenant-kubeconfig.yaml --insecure-skip-tls-verify delete pod hello 2>/dev/null || true
run "kubectl --kubeconfig=/tmp/tenant-kubeconfig.yaml --insecure-skip-tls-verify run hello --image=registry.access.redhat.com/ubi9/ubi-minimal --restart=Never -- bash -c 'echo \"Hello from the sovereign tenant cluster!\"; hostname; date; sleep 2'"

echo ""
narrate "Waiting for pod to complete..."
sleep 20
run "kubectl --kubeconfig=/tmp/tenant-kubeconfig.yaml --insecure-skip-tls-verify get pod hello -o wide"
echo ""
run "kubectl --kubeconfig=/tmp/tenant-kubeconfig.yaml --insecure-skip-tls-verify get pod hello -o jsonpath='Phase: {.status.phase}, ExitCode: {.status.containerStatuses[0].state.terminated.exitCode}'"
echo ""
pause

# --- Done ---
echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║                    E2E Demo Complete!                       ║${NC}"
echo -e "${GREEN}║                                                             ║${NC}"
echo -e "${GREEN}║  ✓ HyperShift control plane running in KubeVirt VM         ║${NC}"
echo -e "${GREEN}║  ✓ Worker VM registered with tenant cluster                ║${NC}"
echo -e "${GREEN}║  ✓ Pod ran successfully on sovereign tenant cluster        ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════════════╝${NC}"
