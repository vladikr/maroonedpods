# MaroonedPods Group Mode — Current Context & Plan Handoff

**Date**: 2026-06-19  
**Branch**: `feature/e2e-tests` (includes merged `feature/warm-pool` + `feature/bootc-k3s-node`)

---

## 1. What Is MaroonedPods

A Kubernetes operator that provides **VM-level isolation for pods** using KubeVirt. When a pod is labeled `maroonedpods.io/maroon: "true"`, it gets:
- A scheduling gate (held until a VM is ready)
- A dedicated KubeVirt VMI (VirtualMachineInstance) that boots as a cluster node
- The pod schedules onto that VM node

Three components: **operator** (lifecycle), **server** (admission webhook), **controller** (gate controller + VMI lifecycle).

---

## 2. The Goal: Group Mode for OSAC

### Problem
OSAC needs **sovereign control plane isolation** — HyperShift control plane pods (etcd, kube-apiserver, kube-controller-manager, etc.) for each tenant should run inside VMs for hardware-level isolation. Currently MaroonedPods only supports 1:1 (one VM per pod), which is wasteful for a control plane with many pods.

### Solution
**Group Mode**: Multiple pods sharing a `maroonedpods.io/group` label share a **single VM** instead of each getting a dedicated VM. All control plane pods for tenant X land on one shared VM node.

### Phasing
- **Phase 1 (current)**: Group/pool logic + label-based detection. "Good enough" networking. No new CRDs.
- **Phase 2 (future)**: OSAC unified networking integration (Netris, multi-fabric, InfiniBand, NVLink). Out of scope now.

---

## 3. Key Design Decisions (Confirmed)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| VMs per group | **1 VM per group** | Simplest model for Phase 1. Each unique group ID gets exactly one VM. |
| VM join method | **kubeadm** (not k3s) | Aligns with future `marooned-node-ocp` image. Existing warm pool also uses kubeadm. |
| Group VM sizing | **Separate `GroupBaseVMResources`** | Dedicated config fields (higher defaults: 4 CPU, 8Gi) since multiple pods share. |
| Group detection | **Label `maroonedpods.io/group`** | Webhook translates `hypershift.openshift.io/cluster` → `maroonedpods.io/group` |
| New CRDs | **None** | Labels + MaroonedPodsConfig only |
| Modes | **Side by side** | No group label → 1:1 (unchanged). Has group label → group mode. |
| Pool size config | **Not needed** | 1 VM per group makes pool size moot |
| Boot script | **Unchanged** | Group VMs use kubeadm cloud-init (inline in controller). k3s boot script stays for 1:1. |

---

## 4. Implementation Status: COMPLETE

All code is written and compiles cleanly. Changes span **6 modified files + 1 new file** (607 lines added).

### Files Changed

| File | What Changed |
|------|-------------|
| `pkg/util/util.go` | Added constants: `GroupLabel`, `HypershiftClusterLabel`, `GroupPoolStateLabel`, `GroupVMNamePrefix`, `GroupNodeLabel`, `GroupPoolStateCreating`, `GroupPoolStateReady` |
| `staging/.../v1alpha1/types.go` | Added `GroupNodeImage`, `GroupBaseVMResources` to `MaroonedPodsConfigSpec`. Added `GroupPools map[string]GroupPoolStatus` to status. Added `GroupPoolStatus` struct. |
| `staging/.../zz_generated.deepcopy.go` | Added deepcopy for `GroupPoolStatus`. Updated `MaroonedPodsConfigSpec` and `MaroonedPodsConfigStatus` deepcopy for new fields. |
| `pkg/maroonedpods-server/handler/handler.go` | Added `getGroupName()` (detects group from canonical or HyperShift label), `mutateGroupPod()` (group-level nodeSelector/toleration/label injection). Modified `Handle()` to route. |
| `pkg/.../maroonedpods-gate-controller.go` | Added `executeGroup()`, `createGroupVMI()`, `markGroupVMIReady()`, `handleGroupPodDeletion()`, `cleanupGroupVM()`, `countGroupPods()`, `reconcileGroupPools()`, `getGroupVMI()`, `getGroupVMResourcesFromConfig()`, `sanitizeGroupName()`, `updateGroupPoolStatus()`, `removeGroupPoolStatus()`. Modified `execute()`, `deletePod()`, `Run()`. |
| `tests/builders/pod.go` | Added `WithGroupLabel()`, `WithHypershiftClusterLabel()`, `NewGroupPod()`, `NewHypershiftPod()` |
| `tests/e2e_group_mode_test.go` | **New file**: 8 e2e test cases for group mode |

### How the Code Works

**Webhook path** (for pods with `maroonedpods.io/maroon` + group label):
1. `Handle()` detects group pod → `mutateGroupPod()`
2. Adds scheduling gate + finalizer (same as 1:1)
3. Sets `nodeSelector: {maroonedpods.io/group-node: <groupName>}` (not pod-name-based)
4. Sets `toleration: {key: maroonedpods.io/group, value: <groupName>, effect: NoSchedule}`
5. Injects `maroonedpods.io/group` label if only HyperShift label was present

**Controller path** (for gated group pods):
1. `execute()` detects group label → `executeGroup()`
2. `executeGroup()` looks for existing VMI with `maroonedpods.io/group=<name>`
3. If no VMI → `createGroupVMI()` (kubeadm join, group labels, deterministic name `maroonedpods-group-<name>`)
4. If VMI not Running → backoff
5. If Running + node joined → `markGroupVMIReady()` (labels/taints node) → `releasePod()`
6. On pod deletion → `handleGroupPodDeletion()`: remove finalizer, if last pod → `cleanupGroupVM()`
7. Background `reconcileGroupPools()` (60s) catches stale groups with no pods

---

## 5. Cluster & Deployment Context

### Target Cluster
- **OpenShift 4.22.1** on OpenStack (PSI)
- **API**: `api.virt-den-422b.rhos-psi.cnv-qe.rhood.us:6443`
- **Kubeconfig**: `~/Downloads/kubeconfig-m`
- **Nodes**: 3 masters (8 CPU, 15Gi), 3 workers (12 CPU, 22Gi)
- **Instance types**: `cnv.psi-prod.m1.xlarge` / `cnv.psi-prod.m1.xlarge.xmem` (nested virt capable)
- **Storage**: Ceph RBD, CephFS, Cinder CSI, Manila
- **Currently installed**: Nothing extra (no CNV, no MCE, no MaroonedPods)

### What Needs to Be Installed
1. **CNV/KubeVirt** — prerequisite for MaroonedPods VMIs
2. **MaroonedPods** — our code (build images, push to quay.io/vladikr, deploy)
3. **MCE** (Multicluster Engine) — provides HyperShift capability
4. **HyperShift hosted cluster** — for real integration testing

### OSAC Installer Manifests (reusable)
Located at `~/devel/osac-project/osac-workspace/osac-installer/prerequisites/`:
- `cnv/cnv-operator.yaml` — Namespace + OperatorGroup + Subscription for `kubevirt-hyperconverged`
- `cnv/cnv-config.yaml` — HyperConverged CR (empty spec)
- `mce/mce-operator.yaml` — Namespace + OperatorGroup + Subscription for `multicluster-engine`
- `mce/mce-config.yaml` — AgentServiceConfig (needs storage: 10Gi DB, 20Gi FS, 50Gi images)

### MaroonedPods Build/Deploy
- **Registry**: `quay.io/vladikr` (push access confirmed)
- **Build**: `make build` (Go binaries), then `DOCKER_PREFIX=quay.io/vladikr DOCKER_TAG=group-mode make push`
- **3 images**: `maroonedpods-controller`, `maroonedpods-server`, `maroonedpods-operator`
- **Deploy manifest**: `manifests/generated/operator-everything.yaml.in` (CRDs + RBAC + Deployments + Webhook)
- **Node image**: `quay.io/vladikr/marooned-node:latest` (bootc + k3s, for 1:1 mode)

### OSAC HyperShift Architecture (from osac-workspace)
- HyperShift is NOT installed standalone — it comes with MCE
- OSAC creates hosted clusters via: `ClusterOrder` CR → `osac-operator` → AAP (Ansible) → `HostedCluster` + `NodePool` CRs
- Platform type: `Agent` (bare metal agent-based provisioning)
- For our testing, we can create `HostedCluster` manually (don't need full OSAC stack)

---

## 6. Next Steps (Deployment & Testing)

### Immediate: Install everything and test
1. Apply CNV operator subscription → wait for CSV → apply HyperConverged CR
2. Build MaroonedPods images with group mode code → push to quay.io/vladikr
3. Deploy MaroonedPods to cluster (operator-everything.yaml + MaroonedPods CR + MaroonedPodsConfig)
4. Test group mode with labeled pods (simulated control plane)
5. Apply MCE operator subscription → wait for CSV → apply AgentServiceConfig
6. Create a HyperShift hosted cluster for real integration test

### Test Cases
- Pods with `maroonedpods.io/group=test-cp` → single VMI created, all pods ungated when ready
- Pods with `hypershift.openshift.io/cluster=tenant-1` → group label injected by webhook
- Regular `maroonedpods.io/maroon` pod (no group) → 1:1 VMI (regression check)
- Delete one group pod → VMI stays
- Delete all group pods → VMI cleaned up

---

## 7. Open Questions

1. **MCE storage**: AgentServiceConfig needs PVCs (10Gi + 20Gi + 50Gi). Need to ensure a default StorageClass is set or specify one explicitly. Cluster has `standard-csi` (Cinder) and `ocs-storagecluster-ceph-rbd`.

2. **HyperShift hosted cluster creation**: OSAC uses AAP/Ansible with Agent platform type. For manual testing, we may need to use `hypershift` CLI or create HostedCluster/NodePool manually. KubeVirt platform type might be simpler for testing (VMs as workers instead of bare metal agents).

3. **Node image for group VMs**: Currently using the default container disk (`quay.io/capk/ubuntu-2004-container-disk:v1.26.0`) for group VMs since they use kubeadm. This needs to be a kubeadm-capable image. The k3s bootc image is for 1:1 mode only.

4. **Webhook configuration**: The webhook MutatingWebhookConfiguration needs to match the cluster's namespace configuration. The `namespaceSelector` on the MaroonedPods CR determines which namespaces are intercepted.
