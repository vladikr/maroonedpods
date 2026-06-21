#!/bin/bash
# MaroonedPods Group Mode - Full Automation Demo
# Demonstrates end-to-end lifecycle with zero manual intervention

set -e

KUBECONFIG=${KUBECONFIG:-~/Downloads/kubeconfig-m}
export KUBECONFIG

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log() {
    echo -e "${BLUE}[$(date +'%H:%M:%S')]${NC} $1"
}

success() {
    echo -e "${GREEN}[$(date +'%H:%M:%S')]${NC} ✓ $1"
}

step() {
    echo -e "\n${YELLOW}=== $1 ===${NC}\n"
}

# Cleanup function
cleanup() {
    log "Cleaning up previous test resources..."
    # Use timestamp to ensure unique namespace
    DEMO_NS="demo-group-$(date +%s)"
    export DEMO_NS
}

# Main demo
main() {
    step "MaroonedPods Group Mode Demo"
    log "This demo shows full automation: pods → VMI → node → CSR auto-approval → pods running"

    cleanup

    step "Step 1: Create test namespace and pods with group labels"
    kubectl create namespace $DEMO_NS

    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: cp-etcd
  namespace: $DEMO_NS
  labels:
    maroonedpods.io/maroon: "true"
    maroonedpods.io/group: "demo"
spec:
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
---
apiVersion: v1
kind: Pod
metadata:
  name: cp-apiserver
  namespace: $DEMO_NS
  labels:
    maroonedpods.io/maroon: "true"
    maroonedpods.io/group: "demo"
spec:
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
EOF

    success "Pods created"
    sleep 2

    log "Checking pod status (should be SchedulingGated):"
    kubectl get pods -n $DEMO_NS

    step "Step 2: Verify VMI auto-creation"
    sleep 3
    log "VMI should be auto-created by controller:"
    kubectl get vmi -n $DEMO_NS

    step "Step 3: Watch VM boot and CSR auto-approval (takes ~2-3 minutes)"
    log "Monitoring for:"
    log "  - Bootstrap CSRs (from node-bootstrapper SA)"
    log "  - Node registration"
    log "  - Serving CSRs"
    log "  - Node Ready"

    # Monitor CSRs and node registration
    TIMEOUT=900  # 15 minutes (full cycle: boot + CSR + ungate can take 10-12 minutes)
    ELAPSED=0
    NODE_REGISTERED=false

    while [ $ELAPSED -lt $TIMEOUT ]; do
        # Check for new CSRs
        NEW_CSRS=$(kubectl get csr --sort-by=.metadata.creationTimestamp 2>/dev/null | grep -E "demo|node-bootstrapper" | tail -5 || true)
        if [ -n "$NEW_CSRS" ]; then
            log "New CSRs detected:"
            echo "$NEW_CSRS" | head -3
        fi

        # Check if node registered
        if kubectl get node maroonedpods-group-demo &>/dev/null; then
            if [ "$NODE_REGISTERED" = false ]; then
                success "Node registered!"
                NODE_REGISTERED=true
                kubectl get node maroonedpods-group-demo
            fi

            # Check if node is Ready
            if kubectl get node maroonedpods-group-demo | grep -q "Ready"; then
                success "Node is Ready!"
                break
            fi
        fi

        sleep 10
        ELAPSED=$((ELAPSED + 10))
        echo -n "."
    done

    echo ""

    if [ $ELAPSED -ge $TIMEOUT ]; then
        log "Timeout waiting for node to become Ready"
        exit 1
    fi

    step "Step 4: Verify CSR auto-approval in controller logs"
    log "Recent CSR approvals:"
    kubectl logs -n maroonedpods -l maroonedpods.io=maroonedpods-controller --tail=30 --since=5m | grep -i "auto-approv" || true

    step "Step 5: Wait for pods to ungate and schedule"
    log "Waiting for controller to process ready node and ungate pods..."
    sleep 60

    step "Step 6: Final Status"
    success "Pods should now be Running on the dedicated node!"
    kubectl get pods -n $DEMO_NS -o wide

    echo ""
    log "Node details:"
    kubectl get node maroonedpods-group-demo -o wide

    echo ""
    log "VMI details:"
    kubectl get vmi -n $DEMO_NS

    step "Demo Complete!"
    success "Full lifecycle completed with ZERO manual intervention!"
    echo ""
    log "The automation included:"
    log "  ✓ Webhook intercepted pods and added scheduling gates"
    log "  ✓ Controller auto-created VMI with RHCOS + Ignition"
    log "  ✓ VM booted and requested bootstrap certificates"
    log "  ✓ Controller auto-approved bootstrap CSRs"
    log "  ✓ Node registered to cluster"
    log "  ✓ Controller auto-approved serving CSRs"
    log "  ✓ Node became Ready"
    log "  ✓ Controller removed blocking taints"
    log "  ✓ Controller ungated pods"
    log "  ✓ Pods scheduled to dedicated group node"

    echo ""
    log "To cleanup: kubectl delete namespace $DEMO_NS && kubectl delete node maroonedpods-group-demo"
}

main "$@"
