# Session State / Handoff Document
## MaroonedPods HyperShift E2E — 2026-07-14 through 2026-07-21

---

## 1. OVERALL GOAL

Run a fully virtualized sovereign tenant cluster using MaroonedPods + HyperShift:
- **Layer 1 (DONE):** CP VM on management cluster → runs HyperShift control plane
- **Layer 2 (DONE — proven, needs stabilization):** Worker VM joins tenant cluster → runs pods
- **Full E2E (PROVEN):** Pod `hello` ran and completed on the tenant cluster worker node

---

## 2. CURRENT STATE (2026-07-21)

### Cluster: virt-den-422
- OCP 5.0ec3, KubeVirt 1.8.4, MCE 2.17
- Kubeconfig: `~/devel/kubeconfig`
- 3 masters + 3 workers (30Gi each)

### HostedCluster: tenant-clean
- **Just recreated with OCP 4.20.0** (`quay.io/openshift-release-dev/ocp-release:4.20.0-x86_64`)
- Previous HC used 4.17.5 which had CPO bug OCPBUGS-56966 causing 10-min cycling
- CPO 4.20 is STABLE — kapi pod unchanged for 18+ minutes
- 38 deployments, 37 running pods, etcd 3/3, kapi 4/4
- EtcdAvailable=True, KubeAPIServerAvailable=True, IgnitionEndpointAvailable=True
- **Available=False** because Route load-balancing picks bridge pod endpoint

### CP VM
- VMI `maroonedpods-group-clusters-tenant-clean` Running on worker-0-fbhs6
- IP: 10.129.2.228 (bridge binding)
- Bridge CNI for internal pods (10.244.0.0/24)
- All OVS/OVN systemd services MASKED
- Auto-proxy script at `/usr/local/bin/auto-proxy.sh` (needs restart after VM reboot)
- socat on :6443 → kapi bridge pod, :8443 → router bridge pod, :9443 → ignition-server-proxy bridge pod

### Worker VM (NodePool)
- NOT yet created on 4.20 HC (NodePool needs to be added)
- Previous worker on 4.17 HC: registered as Ready, pod ran successfully
- NodePool config: `worker-ovs-mask` ConfigMap in `clusters` namespace with OVS masks + bridge CNI

### Route Issue (CURRENT BLOCKER)
- The OCP router load-balances between our socat proxy endpoint (working) and the bridge pod endpoint (unreachable)
- ~50% of requests fail
- Direct access to socat proxy works: `curl -sk https://10.129.2.228:6443/healthz` → `ok`
- Fix needed: suppress bridge pod from kapi Service EndpointSlice, OR deploy proxy pod on management cluster

---

## 3. ARCHITECTURAL DECISIONS + REASONS

### Bridge binding (not masquerade, not passt)
- **Masquerade:** kubelet registers with internal IP 10.0.2.2 (unreachable from API server), oc logs fails
- **Passt:** cross-node traffic blocked by OVN br-ex drop rules, geneve tunnels broken when OVS masked
- **Bridge:** VM gets pod IP directly, routable from anywhere. Requires `kubevirt.io/allow-pod-bridge-network-live-migration` annotation for OVN-K MAC handling

### Mask ALL OVS/OVN systemd services
- RHCOS boots full OVN stack (ovs-vswitchd, ovn-controller, ovnkube-node)
- Creates br-ex with `priority=103` drop rule blocking pod CIDR traffic
- ovnkube-node DaemonSet re-syncs rules if manually deleted
- dpu-host label evicts DaemonSet but ovn-controller stops → geneve breaks
- **Solution:** mask 16 services in ignition + use bridge CNI instead

### SELinux container_file_t (not container_var_lib_t)
- CRI-O storage symlinked to data disk
- Without `container_file_t`, containers get AVC denied on libc.so.6 → exit 127
- The `crio-storage-setup.service` in ignition must use `container_file_t`

### OCP 4.20 release (not 4.17)
- 4.17 CPO has OCPBUGS-56966: ignition-server and ignition-server-proxy fight over same Route
- Causes CPO to cycle, deleting/recreating all deployments every ~10 min
- PKI rotates each cycle → worker kubelet bootstrap certs invalidated
- 4.20 fix (PR#6225): consolidated Route ownership under ignition-server component

### NodePool MachineConfig for worker OVS masking
- Worker VM ignition comes from HyperShift's ignition server (not our prepare-ignition.sh)
- NodePool `spec.config` references ConfigMap `worker-ovs-mask` with MachineConfig
- MachineConfig masks OVS services + adds bridge CNI config

---

## 4. CODE CHANGES (on feature/e2e-tests branch, commit 83995a1)

### Files modified:
1. **`hack/prepare-ignition.sh`** (+40 lines)
   - Added `crio-storage-setup.service` (symlinks container storage to data disk with `container_file_t`)
   - Fixed DNAT sync to get VM IP from `ip addr show enp1s0` (not DNS hostname)

2. **`pkg/maroonedpods-controller/maroonedpods-gate-controller/maroonedpods-gate-controller.go`** (+63/-7 lines)
   - Primary interface: masquerade → bridge binding
   - Added `kubevirt.io/allow-pod-bridge-network-live-migration` annotation
   - Added `ensureVirtLauncherNetworkPolicy()` — creates allow-all-ingress NetworkPolicy
   - Added MCD cordon fix in `markGroupVMIReady()` — patches node annotations
   - Data disk: 50Gi → 100Gi
   - Removed MTU hook sidecar annotation

3. **`pkg/maroonedpods-server/handler/handler.go`** (-12 lines)
   - Removed `maroonedpods.io/vmi-cleanup` finalizer from `mutateGroupPod()`
   - Prevents ghost pod accumulation (200+ per cycle → disk pressure → CP collapse)

### NOT committed but needed:
- `worker-ovs-mask` ConfigMap (applied manually to `clusters` namespace)
- Auto-proxy script (deployed manually inside CP VM)
- IngressController routeSelector removal (applied manually)
- node-pod-reader ClusterRole/ClusterRoleBinding (applied manually)

### Git push BLOCKED:
- Commit `358010b` has `images/node/output-ocp/qcow2/disk.qcow2` (1.7GB) in history
- Fix: `git filter-repo --path images/node/output-ocp/qcow2/disk.qcow2 --invert-paths`
- Then force-push

---

## 5. KEY CONSTRAINTS & NON-NEGOTIABLES

- **NEVER force-delete pods/PVCs** — creates orphaned objects
- **NEVER remove signer Secret finalizers** — cascades into HCP deletion, CPO crash, complete CP collapse
- **NEVER DNAT OUTPUT chain** for passt VM — breaks management cluster etcd
- **ConfigDrive** for ignition delivery (not CloudInitNoCloud) — RHCOS KubeVirt requires it
- **Container storage must be on data disk** with `container_file_t` SELinux — 16G root disk fills immediately
- **Bridge CNI config** must be in `/etc/kubernetes/cni/net.d/` (not just `/etc/cni/net.d/`)

---

## 6. REJECTED APPROACHES

| Approach | Why rejected |
|----------|-------------|
| Passt binding | OVN br-ex drop rules block cross-node traffic |
| Masquerade binding | kubelet registers with 10.0.2.2, unreachable from API server |
| dpu-host label | ovnkube variant crashes (needs DPU hardware) |
| dpu label (no ovnkube) | OVN routing breaks (no port management) |
| l2bridge + UDN | UDN isolated from pod network |
| OVS flow guard (clean drop rules) | ovnkube-node re-syncs faster than guard |
| MutatingAdmissionPolicy for worker VMI | ApplyConfiguration can't mutate atomic arrays |
| Protecting signer Secrets with finalizers | CPO can't delete them → cascading failures |

---

## 7. OPEN PROBLEMS / BUGS / UNRESOLVED

### P0: Route load-balancing
OCP router picks between socat proxy endpoint (working) and bridge pod endpoint (unreachable). ~50% failures. Need to suppress bridge pod from EndpointSlice.

**Options:**
- Deploy a proxy Deployment on management cluster with `app: private-router` label (tried, caused CPO issues on 4.17 — might work on 4.20)
- Change Service selector to not match bridge pod (CPO reverts it)
- Use `externalTrafficPolicy: Local` on router Service

### P1: Auto-proxy reliability
The auto-proxy script (`/usr/local/bin/auto-proxy.sh`) sometimes doesn't start properly after CRI-O restart. The `setsid socat ... &>/dev/null &` subprocess spawning is fragile. Needs to be a proper systemd service in the ignition.

### P2: Worker VM not yet deployed on 4.20 HC
Need to add NodePool with `worker-ovs-mask` ConfigMap config. The ConfigMap exists in `clusters` namespace. Also need the Layer2 CUDN `maroonedpods-l2` and the KubevirtMachineTemplate secondary interface patch.

### P3: kubelet --node-ip empty
`nodeip-configuration.service` is masked → kubelet starts with `--node-ip=` (empty) → node has no InternalIP → `oc logs` fails. MCD writes the flag literally, not from env var. Need a service that writes the IP to kubelet.service ExecStart.

### P4: etcd PVC stale on VMI recreation
Every VMI deletion + recreation loses emptyDisk data. HPP PVs reference stale paths. Must manually delete PVC/PV. Controller should automate this.

---

## 8. IMPORTANT FILE PATHS & CONFIGS

### On the management cluster
- Kubeconfig: `~/devel/kubeconfig`
- Tenant kubeconfig: `/tmp/tenant-kubeconfig.yaml` (regenerate: `kubectl get secret -n clusters-tenant-clean admin-kubeconfig -o jsonpath='{.data.kubeconfig}' | base64 -d > /tmp/tenant-kubeconfig.yaml`)
- HC manifest: `/tmp/hc-4.20.yaml`
- Auto-proxy script: `/tmp/auto-proxy.sh`
- Worker MachineConfig: `worker-ovs-mask` ConfigMap in `clusters` namespace
- Ignition generator: `hack/prepare-ignition.sh`

### Inside CP VM
- Ignition secret: `worker-ignition-full` in `maroonedpods` namespace
- Auto-proxy: `/usr/local/bin/auto-proxy.sh`
- Auto-proxy log: `/var/log/auto-proxy.log`
- socat targets: `/tmp/kapi_ip`, `/tmp/ign_ip`, `/tmp/router_ip`
- CRI-O storage symlink: `/var/lib/containers/storage → /var/hpp-csi-local-basic/containers-storage`
- Console access: `virsh set-user-password` then `virtctl console` with pexpect

### Key functions in controller
- `createGroupVMI()` — creates bridge-binding VMI with annotation
- `markGroupVMIReady()` — labels/taints node, fixes MCD, creates NetworkPolicy, cleans ghost pods
- `ensureEndpointSlices()` — updates proxy EndpointSlices (runs every 30s)
- `ensureVirtLauncherNetworkPolicy()` — creates allow-all-ingress NetworkPolicy
- `ensureIgnitionSecret()` — copies ignition to tenant namespace
- `mutateGroupPod()` — webhook: adds gate + nodeSelector + tolerations (NO finalizer now)

### Key RBAC
- `node-pod-reader` ClusterRole — grants `system:nodes` pod list permission (for auto-proxy kubectl)
- `maroonedpods-endpointslice` ClusterRole — grants controller EndpointSlice access

### Key secrets/configmaps
- `ignition-server-proxy` Service — on 4.17, needed finalizer `maroonedpods.io/keep-alive` to prevent CPO from deleting. On 4.20, NOT needed (bug fixed)
- `root-ca` Secret/ConfigMap — the tenant cluster's root CA
- `admin-kubeconfig` Secret — tenant admin kubeconfig
- `worker-ignition-full` Secret — CP VM ignition in `maroonedpods` namespace

---

## 9. CRITICAL GOTCHAS / INSIGHTS

1. **CPO OCPBUGS-56966** is the #1 stability blocker on OCP ≤4.19. Fixed in 4.20 (PR#6225).
2. **Bridge binding MAC swap** — KubeVirt changes eth0's MAC, gives VM the original. OVN-K needs `allow-pod-bridge-network-live-migration` annotation.
3. **CAPI Machine objects** use `cluster.x-k8s.io` API group, NOT `machine.openshift.io`. Use `kubectl get machine.cluster.x-k8s.io` to find them.
4. **HyperShift creates a private router** (MetalLB VIP 192.168.2.200) for ALL Routes. Default OCP router excludes them via `routeSelector`. Remove the selector.
5. **The crio-storage-setup service** must run Before=crio and use `container_file_t` (not `container_var_lib_t`).
6. **etcd encryption key** must be raw 32 bytes: `openssl rand 32` piped to `--from-file=key=`, NOT base64-encoded.
7. **Console access via pexpect**: `virsh set-user-password` then `virtctl console` with pexpect automation. Use `PROMPT = r'[\$#] '` to match both prompts.
8. **Worker VM ignition** uses merge source from ignition server Route. OVS mask comes from NodePool MachineConfig. Bridge CNI comes from MachineConfig files section.
9. **Pod CIDR** for bridge CNI inside VM is 10.244.0.0/24 (host-local IPAM). This is separate from the management cluster's pod CIDR (10.128.0.0/14).

---

## 10. PRECISE NEXT STEPS

### Immediate (to get full E2E on 4.20):
1. **Fix Route load-balancing** — deploy proxy pod or suppress bridge pod endpoint
2. **Add NodePool** — scale to 1 with `worker-ovs-mask` config
3. **Verify worker registers** — should be stable with 4.20 CPO (no PKI rotation)
4. **Run a pod** on the tenant cluster

### Short-term:
5. **Automate manual hacks** — add to controller/ignition:
   - Auto-proxy as systemd service in ignition
   - IngressController routeSelector patch in controller
   - node-pod-reader RBAC in controller
   - PVC cleanup in controller
6. **Push PR** — fix git history (remove qcow2), push feature/e2e-tests
7. **Write architecture document** — full flow from HC creation to pod running

### Medium-term:
8. **Create demo script** — automated E2E showing the full flow
9. **Investigate MutatingAdmissionPolicy** for worker VMI secondary interface (CEL JSONPatch value syntax)
10. **Fix kubelet --node-ip** — create-node-env service that writes IP to kubelet.service

---

## OCP 4.20 E2E COMPLETE (2026-07-21)

### Worker node registered and Ready
- HC `tenant-clean` recreated with OCP 4.20.0 release (fixes CPO OCPBUGS-56966)
- CPO is STABLE — no more 10-minute deployment cycling
- NodePool created with `worker-ovs-mask` ConfigMap (OVS masks + bridge CNI)
- CAPI created worker VMI, booted RHCOS, fetched ignition
- Worker node `tenant-clean-mf24t-4bvxb` registered as **Ready**, v1.33.5, CRI-O 1.33.4
- Pod `hello2` ran and completed (exit code 0) on the tenant cluster

### Key fixes applied
1. **EndpointSlice port name "https":** Proxy EndpointSlices for `ignition-server-proxy` and `router` needed port name `"https"` to match the Service `targetPort`. Without this, HAProxy ignored the proxy endpoints entirely — traffic went only to bridge pod endpoints (unreachable).
2. **socat startup for ports 8443 and 9443:** socat for ports 8443 (router) and 9443 (ignition-server-proxy) needed to be started manually on the CP VM. The auto-proxy script was not running as a systemd service.
3. **ClusterImagePolicy fix (gotcha #20/#28):** `ocp-v4.0-art-dev` scope had to be removed from ClusterImagePolicy — directly edited `policy.json` on the worker node and restarted CRI-O.

### Remaining issues
- **kubectl logs doesn't work:** konnectivity-agent not connecting (konnectivity-server Service routing issue)
- **HC Available=False:** MetalLB VIP (192.168.2.200) health check fails because private router is a bridge pod inside the CP VM — unreachable from outside
- **Auto-proxy not a systemd service:** socat proxies still require manual startup after VM reboot; needs to be converted to a proper systemd unit in the ignition
