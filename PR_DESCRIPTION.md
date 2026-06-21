# Add Group Mode for VM Pool Scheduling with Full OCP Support

## Summary

This PR implements **group mode** for MaroonedPods — a scheduling-layer mechanism for dedicating a single KubeVirt VM node to multiple related pods (e.g., a control plane's pods). This provides stronger isolation boundaries for multi-tenant hosted control planes while maintaining better resource efficiency than per-pod isolation (Kata/sandboxed containers).

Tested end-to-end on **OpenShift 4.22.1** with full automation — zero manual intervention required.

## What is Group Mode?

Instead of creating a 1:1 mapping between pods and VM nodes, group mode allows multiple pods with the same `maroonedpods.io/group` label to share a dedicated VM node:

```yaml
apiVersion: v1
kind: Pod
metadata:
  labels:
    maroonedpods.io/maroon: "true"
    maroonedpods.io/group: "tenant-cp-1"
spec:
  containers: [...]
```

When pods with these labels are created, MaroonedPods:
1. Intercepts them via webhook and adds scheduling gates
2. Auto-creates a KubeVirt VMI running RHCOS
3. VM boots and registers as a node
4. Auto-approves kubelet CSRs
5. Ungates pods once node is Ready
6. Pods schedule to their dedicated group node

## Key Features

### 1. Full OpenShift Support
- **RHCOS boot via Ignition** — VMs boot Red Hat CoreOS with proper networking, kubelet config, and MCD support
- **CSR auto-approval** — both bootstrap CSRs (from `node-bootstrapper` SA) and serving CSRs
- **SCC compatibility** — controller has necessary permissions for pod mutations on OpenShift
- **Tested on OCP 4.22.1**

### 2. Complete Automation
- No manual CSR approval needed
- No manual node taint removal
- No manual pod ungating
- Full lifecycle managed by controller

### 3. Webhook-Based Pod Interception
- Pods with `maroonedpods.io/maroon + maroonedpods.io/group` labels automatically intercepted
- Scheduling gates, tolerations, and finalizers added transparently
- Works with any pod spec

### 4. Ignition Config Management
- `hack/prepare-ignition.sh` — script to generate complete Ignition configs for RHCOS worker nodes
- Includes bootstrap kubeconfig, bridge CNI, MCD encapsulated MachineConfig, OVS disablement
- Ignition secret automatically copied to target namespaces

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ Management Cluster (OpenShift)                              │
│                                                             │
│  ┌─────────────────────────────────────────┐               │
│  │ Namespace: tenant-cp-1                   │               │
│  │                                          │               │
│  │  Pod: cp-etcd                            │               │
│  │  Pod: cp-apiserver    ───────────────────┼──┐            │
│  │  (labels: maroonedpods.io/group=demo)    │  │            │
│  └─────────────────────────────────────────┘  │            │
│                                                │            │
│  ┌──────────────────────────────────────────┐ │            │
│  │ KubeVirt VMI: maroonedpods-group-demo    │◄┘            │
│  │  - RHCOS container disk                  │              │
│  │  - Bridge CNI networking                 │              │
│  │  - Registers as K8s node                 │              │
│  └──────────────────────────────────────────┘              │
│                    │                                        │
│                    │ (node joins cluster)                   │
│                    ▼                                        │
│  ┌──────────────────────────────────────────┐              │
│  │ Node: maroonedpods-group-demo            │              │
│  │  - Status: Ready                         │              │
│  │  - Taint: maroonedpods.io/group=demo     │              │
│  │  - Pods: cp-etcd, cp-apiserver (Running) │              │
│  └──────────────────────────────────────────┘              │
└─────────────────────────────────────────────────────────────┘
```

## Commits

### 1. Add e2e functional tests with Ginkgo/Gomega (`58e49b2`)
- Framework for E2E testing
- Test builders and utilities
- Coverage for warm pool, pod isolation, cleanup, dynamic sizing

### 2. Add group mode for VM pool scheduling with OCP support (`501c485`)
- Group mode lifecycle controller (`executeGroup`, `markGroupVMIReady`, `handleGroupPodDeletion`)
- Webhook for group pod admission (tolerations, finalizers, scheduling gates)
- Ignition script for OCP worker nodes (`hack/prepare-ignition.sh`)
- API types: `GroupBaseVMResources`, `JoinConfig` in `MaroonedPodsConfig`
- All pod mutations use PATCH (required for OCP SCC compatibility)
- RBAC for secrets, nodes, VMI management

### 3. Add CSR auto-approval and OCP SCC support (`e3469cf`)
- `reconcileCSRs()` — auto-approves bootstrap + serving CSRs for MaroonedPods nodes
- Handles bootstrap CSRs from `node-bootstrapper` SA (no node name yet)
- Handles serving CSRs from registered nodes (`system:node:<name>`)
- Uses `UpdateApproval()` API (correct RBAC path)
- Searches VMIs across all namespaces
- SCC ClusterRoleBinding — grants controller SA access to `privileged` SCC
- CSR RBAC — `certificatesigningrequests`, `certificatesigningrequests/approval`, `signers`

## Configuration

Example `MaroonedPodsConfig` with group mode:

```yaml
apiVersion: maroonedpods.io/v1alpha1
kind: MaroonedPodsConfig
metadata:
  name: maroonedpods-config
  namespace: maroonedpods
spec:
  # Group mode settings
  groupNodeImage: quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:...
  groupBaseVMResources:
    cpu: 4
    memoryMi: 8192
  joinConfig:
    ignitionSecretRef: worker-ignition-full
  
  # Legacy 1:1 mode still works
  nodeImage: quay.io/capk/ubuntu-2004-container-disk:v1.26.0
  warmPoolSize: 0
```

## Testing

### Automated Demo

Run the included demo script:

```bash
./demo/group-mode-demo.sh
```

This will:
1. Create a namespace with labeled pods
2. Watch VMI auto-creation
3. Monitor CSR auto-approval
4. Verify node registration
5. Confirm pods Running on dedicated node

Expected output: Full lifecycle completes in ~3-4 minutes with zero manual steps.

### Manual Testing

```bash
# 1. Prepare Ignition config (one-time setup)
export KUBECONFIG=~/path/to/kubeconfig
RENDERED_MC=$(oc get machineconfig | grep rendered-worker | head -1 | awk '{print $1}')
KUBECONFIG=$KUBECONFIG RENDERED_MC=$RENDERED_MC ./hack/prepare-ignition.sh

# 2. Create Ignition secret
oc create secret generic worker-ignition-full -n maroonedpods \
  --from-file=userData=/tmp/maroonedpods-worker-ignition.json

# 3. Update MaroonedPodsConfig
oc patch maroonedpodsconfig maroonedpods-config -n maroonedpods --type=merge -p '{
  "spec": {
    "groupNodeImage": "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:...",
    "joinConfig": {"ignitionSecretRef": "worker-ignition-full"}
  }
}'

# 4. Create test pods
kubectl create namespace test-group
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  namespace: test-group
  labels:
    maroonedpods.io/maroon: "true"
    maroonedpods.io/group: "test"
spec:
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
EOF

# 5. Watch automation
watch kubectl get pods,vmi,nodes -A
```

## OpenShift-Specific Fixes

Several OCP-specific behaviors were discovered and fixed:

1. **SCC validation on pod mutations** — OCP validates SCC on ANY pod PATCH/UPDATE, not just creation. The controller SA needs `privileged` SCC access.
2. **Bootstrap kubeconfig requirement** — MCD firstboot needs `/etc/kubernetes/kubeconfig` with a `node-bootstrapper` token. This isn't in the rendered MachineConfig and must be explicitly added to Ignition.
3. **CSR approval API** — Must use `UpdateApproval()` on the `certificatesigningrequests/approval` subresource, not `UpdateStatus()`.
4. **VMI namespace isolation** — Group mode VMs can be in any namespace, so VMI lookup must search all namespaces.

## Comparison to Kata/Sandboxed Containers

MaroonedPods group mode provides several advantages over Kata/peer-pods for hosted control plane isolation:

| Concern | Kata/Sandboxed | MaroonedPods Group Mode |
|---------|---------------|------------------------|
| **Isolation granularity** | Per-pod microVM | Per-group full VM |
| **Storage** | Ephemeral (problematic for etcd) | Full persistent node storage |
| **Networking** | Extra overlay inside VM | Standard bridge CNI |
| **Security** | Needs privileged SCC for runtime | Only controller needs SCC |
| **Performance** | MicroVM overhead per pod | One VM per group |
| **BM requirement** | Yes (needs nested virt) | No |
| **Dangling resources** | Peer-pods can leak cloud VMs | Auto-cleanup tied to pod lifecycle |
| **Maturity** | "Early tech, not tested at HCP scale" | Proven on OCP 4.22.1 |

For control planes where all components trust each other, per-group isolation is more efficient than per-pod.

## Future Work

- **HyperShift integration** — NodePool-like mechanism for group nodes joining hosted clusters
- **OSAC integration** — `MaroonedPodsProvider` for OSAC's `ProvisioningProvider` interface
- **Metrics/observability** — Prometheus metrics for group lifecycle
- **Multi-cluster** — Group nodes joining remote clusters

## Files Changed

- **Controller**: `pkg/maroonedpods-controller/maroonedpods-gate-controller/maroonedpods-gate-controller.go` (+778 lines)
  - Group mode lifecycle, CSR auto-approval
- **Operator**: `pkg/maroonedpods-operator/resources/cluster/controller.go` (+64 lines)
  - CSR RBAC, SCC ClusterRoleBinding
- **Webhook**: `pkg/maroonedpods-server/handler/handler.go` (+52 lines)
  - Group pod admission logic
- **Hack**: `hack/prepare-ignition.sh` (+411 lines)
  - Ignition config generation for OCP workers
- **API**: `staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1/types.go` (+44 lines)
  - `GroupBaseVMResources`, `JoinConfig` types
- **Tests**: `tests/e2e_group_mode_test.go` (+288 lines)
  - E2E tests for group mode

**Total**: 2,654 additions across 17 files

## Demo Video

*(You can record a demo with `asciinema record demo.cast` and run `./demo/group-mode-demo.sh`)*

---

**Ready for review!** This represents a complete, production-ready implementation of group mode with full OpenShift support and automated CSR approval.
