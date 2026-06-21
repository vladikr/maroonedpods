# Group Mode Ignition Integration - Implementation Progress

**Date**: 2026-06-20  
**Branch**: feature/e2e-tests  
**Status**: IN PROGRESS - Redeploying with UserDataSecretRef fix

## Context

Fixing node registration for group mode VMs on OCP 4.22.1 (k8s v1.35.5). The core group mode logic works (webhook, VMI creation per group) but VMs can't join the cluster because:
1. Hardcoded join endpoint (`192.168.66.101:6443` - dev IP)
2. Wrong node image (Ubuntu 20.04 with kubelet v1.26, incompatible with k8s v1.35)
3. Wrong join mechanism (kubeadm cloud-init doesn't work for OCP - needs RHCOS + Ignition)

## Solution: Add Ignition Support

Switch from kubeadm/cloud-init to OCP's native Ignition-based worker join mechanism.

### Cluster Resources Available
- Worker Ignition config: `openshift-machine-api/worker-user-data` Secret (points to MCS at `192.168.0.5:22623`)
- RHCOS container disk: `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:759db300fd0e0258b2179aa48de5575baa2e33e6d0b6efebdf36cde5647baa40`
- OCP v1.35.5, RHCOS 9.8

## Code Changes Completed ✓

### 1. API Types - JoinConfig struct
**File**: `staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1/types.go`

Added `JoinConfig` struct (lines 141-148):
```go
type JoinConfig struct {
    // IgnitionSecretRef references a Secret in the maroonedpods namespace containing worker Ignition config
    // The Secret must have a key "userData" with the Ignition JSON
    // On OpenShift, extract with: oc extract -n openshift-machine-api secret/worker-user-data
    // +optional
    IgnitionSecretRef string `json:"ignitionSecretRef,omitempty"`
}
```

Added field to `MaroonedPodsConfigSpec` (line 191):
```go
JoinConfig JoinConfig `json:"joinConfig,omitempty"`
```

### 2. Deepcopy Methods
**File**: `staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1/zz_generated.deepcopy.go`

Added `JoinConfig` deepcopy methods (after GroupPoolStatus, before MaroonedPodsConfigSpec):
- `DeepCopyInto(out *JoinConfig)`
- `DeepCopy() *JoinConfig`

Updated `MaroonedPodsConfigSpec.DeepCopyInto()` to include:
```go
out.JoinConfig = in.JoinConfig
```

**Verification**: `go build ./staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1/...` — PASSES

### 3. Controller - Ignition Helper
**File**: `pkg/maroonedpods-controller/maroonedpods-gate-controller/maroonedpods-gate-controller.go`

Added `getIgnitionConfig()` helper (after sanitizeGroupName, before createGroupVMI):
- Reads MaroonedPodsConfig.Spec.JoinConfig.IgnitionSecretRef
- Fetches Secret from `maroonedpods` namespace (uses `util.DefaultMaroonedPodsNs`)
- Returns Ignition JSON and boolean indicating if Ignition is configured
- Falls back to false if config missing or Secret not found

### 4. Controller - createGroupVMI Updated
**File**: Same as above

Updated VMI creation in `createGroupVMI()`:
- Calls `getIgnitionConfig()` to check for Ignition
- If Ignition available: uses it directly as userData
- If not: falls back to legacy kubeadm cloud-init (for backward compatibility with dev environments)
- Changed volume source:
  - **With Ignition**: Uses `CloudInitConfigDrive` (supports Ignition format)
  - **Without Ignition**: Uses `CloudInitNoCloud` (existing cloud-init behavior)

**Verification**: `go build ./pkg/maroonedpods-controller/maroonedpods-gate-controller/...` — PASSES

## What's Left to Do

### 1. Update warm pool VMI creation (OPTIONAL - only if warm pool will be used on OCP)
**File**: `pkg/maroonedpods-controller/maroonedpods-gate-controller/maroonedpods-gate-controller.go`
**Function**: `createPoolVMI()` (lines ~865-966)

Same pattern as createGroupVMI:
- Call `getIgnitionConfig()`
- Replace hardcoded kubeadm cloud-init with Ignition when available
- Use CloudInitConfigDrive vs CloudInitNoCloud

### 2. Update 1:1 pod VMI creation (OPTIONAL - 1:1 mode uses different k3s-based approach)
**File**: Same as above
**Function**: `createVMIFromPod()` (lines ~1049-1205)

This currently uses k3s + bootc image, not kubeadm. May need different approach:
- Uses `kubernetes.default.svc:6443` endpoint (in-cluster DNS - won't work for VMs outside cluster)
- Uses `quay.io/vladikr/marooned-node:latest` (bootc + k3s v1.28, hardcoded)
- Could switch to Ignition + RHCOS if OCP is the target platform

### 3. Build and deploy updated images
```bash
cd /home/vladikr/devel/maroonedpods
DOCKER_PREFIX=quay.io/vladikr DOCKER_TAG=group-mode-ignition make build-images push-images
```

### 4. Create worker-ignition Secret on cluster
```bash
KUBECONFIG=~/Downloads/kubeconfig-m oc extract -n openshift-machine-api secret/worker-user-data --keys=userData --to=- \
  | oc create secret generic worker-ignition -n maroonedpods --from-file=userData=/dev/stdin
```

### 5. Create/update MaroonedPodsConfig CR
```yaml
apiVersion: maroonedpods.io/v1alpha1
kind: MaroonedPodsConfig
metadata:
  name: maroonedpods-config
spec:
  # RHCOS image from OCP release payload
  nodeImage: "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:759db300fd0e0258b2179aa48de5575baa2e33e6d0b6efebdf36cde5647baa40"
  joinConfig:
    ignitionSecretRef: "worker-ignition"
  groupBaseVMResources:
    cpu: 4
    memoryMi: 8192
```

### 6. Redeploy MaroonedPods
```bash
# Delete existing deployment
kubectl delete deployment -n maroonedpods maroonedpods-controller maroonedpods-server maroonedpods-operator

# Reapply with new images (update image refs in manifest first)
sed 's|group-mode|group-mode-ignition|g' /tmp/maroonedpods-deploy.yaml | kubectl apply -f -
```

### 7. Delete old test VMI and recreate test pods
```bash
# Clean up old test
kubectl delete vmi -n test-group-mode maroonedpods-group-test-cp-1
kubectl delete pods -n test-group-mode cp-etcd cp-apiserver

# Recreate - new VMI will use Ignition
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: cp-etcd
  namespace: test-group-mode
  labels:
    maroonedpods.io/maroon: "true"
    maroonedpods.io/group: "test-cp-1"
spec:
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
---
apiVersion: v1
kind: Pod
metadata:
  name: cp-apiserver
  namespace: test-group-mode
  labels:
    maroonedpods.io/maroon: "true"
    maroonedpods.io/group: "test-cp-1"
spec:
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
EOF
```

### 8. Monitor VMI boot and node join
```bash
# Watch VMI status
kubectl get vmi -n test-group-mode -w

# Watch for new node
kubectl get nodes -w

# Approve CSRs when node registers
oc get csr | grep Pending | awk '{print $1}' | xargs oc adm certificate approve
```

### 9. Verify end-to-end
- VMI boots with RHCOS image
- Node registers with name matching VMI name (`maroonedpods-group-test-cp-1`)
- After CSR approval, node becomes Ready
- Pods get ungated and scheduled to the new node
- Pods run successfully on VM node

## Key Design Decisions

1. **Backward compatibility**: Falls back to kubeadm cloud-init if no Ignition config, so dev environments still work
2. **ConfigDrive vs NoCloud**: ConfigDrive supports both cloud-init and Ignition; NoCloud is cloud-init only
3. **Secret reference vs inline**: Using Secret reference for Ignition because it contains CA cert (sensitive)
4. **Namespace**: Ignition Secret must be in `maroonedpods` namespace (controller's namespace)
5. **Container disk for RHCOS**: Using RHCOS as containerDisk (1-2GB). Alternative would be DataVolume but that requires PVC provisioning.

## Open Questions

1. **Node naming**: Does RHCOS Ignition set hostname to match VMI name? May need to inject hostname into Ignition config.
2. **CSR auto-approval**: For production, need auto-approver for MaroonedPods nodes (testing can approve manually).
3. **Warm pool on OCP**: Should warm pool also use Ignition, or is it dev-only?
4. **1:1 mode on OCP**: Should 1:1 mode switch from k3s to Ignition, or stay k3s-based?

## Files Modified

- `staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1/types.go` — JoinConfig struct added
- `staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1/zz_generated.deepcopy.go` — JoinConfig deepcopy
- `pkg/maroonedpods-controller/maroonedpods-gate-controller/maroonedpods-gate-controller.go` — getIgnitionConfig() helper, createGroupVMI() updated

## Issues Found and Fixed During Deployment

### Issue 1: CRD schema missing new fields
**Problem**: `joinConfig` and `groupBaseVMResources` were silently dropped when creating MaroonedPodsConfig CR
**Fix**: Manually patched the `maroonedpodsconfigs.maroonedpods.io` CRD on-cluster to add the missing field schemas (saved as `/tmp/maroonedpodsconfig-crd.yaml`)

### Issue 2: RHCOS containerDisk failed with old containerDisk method
**Problem**: `failed to find a sourceFile in containerDisk: lstat .../merged/disk: no such file or directory`
**Fix**: Enabled KubeVirt `ImageVolume` feature gate via HCO annotation:
```bash
oc annotate --overwrite -n openshift-cnv hyperconverged kubevirt-hyperconverged \
  kubevirt.kubevirt.io/jsonpatch='[{"op":"add","path":"/spec/configuration/developerConfiguration/featureGates/-","value":"ImageVolume"}]'
```
This changes how KubeVirt mounts container disks (uses Kubernetes ImageVolume instead of overlay). VMI then booted RHCOS successfully.

### Issue 3: MCS not reachable from pod network
**Problem**: Ignition pointer config pointed to MCS at `192.168.0.5:22623` (node network VIP). VMs on pod network (`10.128.x.x`) cannot reach this. Even the ClusterIP `172.30.45.90:22623` returned "Connection refused" because MCS runs with `hostNetwork: true`.
**Fix**: Use the FULL RENDERED worker Ignition config instead of the pointer config. Extract with:
```bash
oc get machineconfig rendered-worker-aa2abdc99a88179c999475bb71c3d9e0 -o jsonpath='{.spec.config}' > /tmp/worker-ignition-full.json
oc create secret generic worker-ignition-full -n maroonedpods --from-file=userData=/tmp/worker-ignition-full.json
```
The full config is ~342KB, self-contained, no need to contact MCS.

### Issue 4: Inline userData exceeds 2048 byte limit
**Problem**: KubeVirt webhook rejected VMI: `cloudInitConfigDrive userdata exceeds 2048 byte limit. Should use UserDataSecretRef for larger data.`
**Fix**: Changed controller to use `UserDataSecretRef` instead of inline `UserData`. The controller now:
1. Copies the Ignition Secret from `maroonedpods` namespace to the VMI's namespace (as `maroonedpods-ignition`)
2. References it via `CloudInitConfigDriveSource.UserDataSecretRef`

### Code changes for Issue 4 (in `maroonedpods-gate-controller.go`):
- Replaced `getIgnitionConfig()` with `ensureIgnitionSecret(targetNamespace)` which:
  - Reads source Secret from maroonedpods namespace
  - Creates/updates a copy named `maroonedpods-ignition` in the VMI's namespace
  - Returns the target Secret name + bool
- Updated `createGroupVMI()` to use `UserDataSecretRef` instead of inline `UserData`

### Issue 5: Controller image not rebuilt with new code
**Problem**: `make build-images push-images` pushed a stale image — the binary inside didn't contain `ensureIgnitionSecret`. The old `make` target was caching.
**Fix**: Used `podman build --no-cache` followed by explicit `podman push` for each image. Verified with `strings` that the binary contains the new code before pushing.

### Issue 6: RBAC — controller can't create Secrets in other namespaces
**Problem**: `secrets is forbidden: User "system:serviceaccount:maroonedpods:maroonedpods-controller" cannot create resource "secrets" in API group "" in the namespace "test-group-mode"`
**Fix**: Patched the controller's ClusterRole to add secrets permissions:
```bash
oc patch clusterrole maroonedpods-controller --type=json \
  -p='[{"op":"add","path":"/rules/-","value":{"apiGroups":[""],"resources":["secrets"],"verbs":["get","create","update"]}}]'
```

### Issue 7: Wrong RHCOS container image — OS filesystem, not VM disk
**Problem**: The `rhel-coreos` image from the OCP release payload (`sha256:759db300...`) is a bootable ostree container (the OS filesystem tree), NOT a KubeVirt containerDisk. It has `/usr`, `/etc`, `/sysroot` but no disk file. KubeVirt's ImageVolume mounted it but virt-handler couldn't find a disk file.
**Fix**: Downloaded the RHCOS qcow2 from the stream metadata and built a proper containerDisk:
```bash
# Get qcow2 URL from machine-os-images stream metadata
curl -L -o /tmp/rhcos-qemu.qcow2.gz "https://rhcos.mirror.openshift.com/art/storage/prod/streams/rhel-9.8/builds/9.8.20260520-0/x86_64/rhcos-9.8.20260520-0-qemu.x86_64.qcow2.gz"
gunzip /tmp/rhcos-qemu.qcow2.gz
# Build containerDisk (FROM scratch, ADD --chown=107:107 rhcos-qemu.qcow2 /disk/)
podman build -t quay.io/vladikr/rhcos-containerdisk:4.22.1 -f /tmp/Dockerfile.rhcos-containerdisk /tmp
podman push quay.io/vladikr/rhcos-containerdisk:4.22.1
```
BUT this qcow2 had `ignition.platform.id=qemu` hardcoded — see Issue 8.

### Issue 8: Ignition platform detection — qemu vs kubevirt
**Problem**: The generic RHCOS qcow2 has `ignition.platform.id=qemu` in the kernel command line. On the `qemu` platform, Ignition reads config from QEMU fw_cfg device (`opt/com.coreos/config`), NOT from config drive. So despite `CloudInitConfigDrive` being correctly configured, Ignition reported "no config provided by user" and "QEMU firmware config was not found. Ignoring..."
**Root cause**: KubeVirt passes Ignition data via config drive (ISO 9660), but the qemu platform Ignition provider doesn't read config drives.
**Fix**: Use the **KubeVirt platform RHCOS containerDisk** from the OCP release payload. Found via:
```bash
# The machine-os-images component has coreos-stream.json with image references
# Stream metadata path: architectures.x86_64.images.kubevirt.digest-ref
```
The KubeVirt RHCOS image is:
```
quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:024cb7de8d9e21ef7fc2bcb90f8dd77e7f742d937822c95e4ec72b9ab48fea7d
```
This image has `ignition.platform.id=kubevirt` and reads Ignition from config drive.

### Issue 9: Missing bootstrap kubeconfig ✓ FIXED
**Problem**: The full rendered MachineConfig Ignition doesn't include `/etc/kubernetes/kubeconfig` (the bootstrap kubeconfig).
**Fix**: Merged bootstrap kubeconfig (node-bootstrapper token + root CA) into Ignition config at `/etc/kubernetes/kubeconfig`.

### Issue 10: Base RHCOS image has no kubelet/CRI-O ✓ FIXED
**Problem**: The KubeVirt RHCOS containerDisk is a BASE image without kubelet or CRI-O. These are normally installed by the MCD firstboot service which rebases the OS using the `machine-os-content` container image.
**Root cause**: The MCD firstboot requires `/etc/ignition-machine-config-encapsulated.json` — a file containing the full rendered MachineConfig. Without it, MCD skips the rebase.
**Fix**: Added the full rendered MachineConfig JSON as `/etc/ignition-machine-config-encapsulated.json` to the Ignition config. MCD firstboot then:
1. Reads the encapsulated MC
2. Pulls `rhel-coreos` image (`sha256:759db300...`) which has kubelet + CRI-O
3. Rebases the OS via rpm-ostree
4. Reboots the VM
After reboot, the VM runs RHCOS `9.8.20260605-0` with kernel `5.14.0-687.13.1` and has CRI-O + kubelet installed.

### Issue 11: OVS/OVN networking breaks VM after rebase ✓ FIXED
**Problem**: The rendered worker MachineConfig includes OVS/OVN/SDN units designed for physical hosts. After rebase, OVS takes over networking (creates `br-ex`, moves NIC), breaking masquerade NAT connectivity. `enp1s0` loses its IP.
**Fix**: Disabled and masked 10 OVS/SDN-related systemd units in both the Ignition config and the encapsulated MC:
- `openvswitch.service`, `ovs-configuration.service`, `ovs-vswitchd.service`, `ovsdb-server.service`
- `wait-for-br-ex-up.service`, `wait-for-ipsec-connect.service`, `ipsec.service`
- `nmstate-configuration.service`, `nmstate.service`, `wait-for-primary-ip.service`
Also removed 4 OVS-related files (`configure-ovs.sh`, `sdn.conf`, etc).
After fix, `enp1s0` retains its `10.0.2.2` IP after rebase/reboot.

### Issue 12: Ignition can't write to /run (read-only in initrd) ✓ FIXED
**Problem**: Added `/run/resolv-prepender-kni-conf-done` as a file in Ignition config. Ignition tried to write to `/sysroot/run/` which is read-only during initrd phase. Caused emergency mode.
**Fix**: Replaced the file with a oneshot systemd service `resolv-prepender-bypass.service` that creates the file at boot time via `ExecStart=/usr/bin/touch /run/resolv-prepender-kni-conf-done`.

### Issue 13: Missing /etc/kubernetes/node.env ✓ FIXED (manually)
**Problem**: Kubelet's `20-node-env.conf` dropin requires `EnvironmentFile=/etc/kubernetes/node.env` (no `-` prefix = mandatory). This file is normally created by `nodeip-configuration.service` (which we masked). Contains `KUBELET_NODE_NAME` and `KUBELET_NODE_IP`.
**Fix applied manually on running VM**: `echo "KUBELET_NODE_NAME=$(hostname)" > /etc/kubernetes/node.env`
**Fix for Ignition**: Added `create-node-env.service` oneshot unit that creates the file at boot:
```
ExecStart=/bin/bash -c 'echo "KUBELET_NODE_NAME=$(hostname)" > /etc/kubernetes/node.env && echo "KUBELET_NODE_IP=$(ip -4 addr show enp1s0 | grep -oP "(?<=inet )\S+" | cut -d/ -f1)" >> /etc/kubernetes/node.env'
```
Also masked `nodeip-configuration.service`, `openstack-kubelet-nodename.service`, `openstack-hostname.service`.

### Issue 14: Missing kubelet-ca.crt ✓ FIXED (manually)
**Problem**: Kubelet config references `clientCAFile: /etc/kubernetes/kubelet-ca.crt` but this file doesn't exist.
**Fix applied manually**: Copied CA from `openshift-kube-apiserver-operator/kube-apiserver-to-kubelet-client-ca` configmap.
**For Ignition**: Need to add this CA cert as `/etc/kubernetes/kubelet-ca.crt` in the Ignition config.

### Issue 15: Wrong CA in bootstrap kubeconfig — x509 errors ✓ FIXED (manually)
**Problem**: Bootstrap kubeconfig had only the cluster `root-ca`, but the API server's TLS certificate is signed by `kube-apiserver-lb-signer` (a separate CA). Kubelet rejected the API server cert: `tls: failed to verify certificate: x509: certificate signed by unknown authority`.
**Fix applied manually**: Replaced the CA in bootstrap kubeconfig with the full API server CA bundle from `openshift-config-managed/kube-apiserver-server-ca` configmap (4 certificates).
**For Ignition**: The bootstrap kubeconfig's `certificate-authority-data` must use this CA bundle, NOT the root-ca alone.
```bash
oc get configmap -n openshift-config-managed kube-apiserver-server-ca -o jsonpath='{.data.ca-bundle\.crt}'
```

### MILESTONE: Node registered! CSRs approved!
After fixing issues 10-15, the node `maroonedpods-group-test-cp-1` successfully:
1. Booted KubeVirt RHCOS with Ignition (`ignition.platform.id=kubevirt`)
2. MCD firstboot rebased OS to include kubelet + CRI-O
3. Kubelet started and generated CSRs
4. CSRs approved → node registered as `worker` role, v1.35.5

### Issue 16: Node is NotReady — CNI not configured (CURRENT)
**Problem**: Node is `NotReady` with: `container runtime network not ready: NetworkReady=false reason:NetworkPluginNotReady message:Network plugin returns error: no CNI configuration file in /etc/kubernetes/cni/net.d/`
**Root cause**: OVN-Kubernetes (the cluster's CNI) is not running on this node because we disabled OVS. Without OVN, there's no CNI config dropped in `/etc/kubernetes/cni/net.d/`.
**Possible approaches**:
1. Enable a minimal CNI (like bridge or host-local) for the VM node
2. Re-enable OVS but configure it to work with masquerade NAT
3. Use a different approach entirely — the VM is inside the pod network already, so maybe a simple bridge CNI pointing to the masquerade interface would work
4. Skip CNI entirely if we just need the node to accept pods that use hostNetwork

### HyperShift Reference Architecture (from research)
HyperShift solves the same problem using:
1. **Local HAProxy reverse proxy** at `172.20.0.1:6443`
2. **Full Ignition payload** (not MCS pointer)
3. **Bootstrap kubeconfig** with full API server CA bundle
4. **KubeVirt platform RHCOS image**
5. OVN runs INSIDE the hosted cluster VMs (separate from the management cluster OVN)

## Current Cluster State (as of 2026-06-20 ~10:30 UTC)

- **Node `maroonedpods-group-test-cp-1`**: Registered, NotReady (CNI missing)
- **VMI**: Running in `test-group-mode`, KubeVirt RHCOS 9.8.20260605-0, kernel 5.14.0-687.13.1
- **Kubelet**: Active and running, CRI-O active, both CSRs approved
- **VM IP**: `10.0.2.2` on `enp1s0` (masquerade NAT)
- **SSH**: Working via `virtctl ssh --known-hosts="" -i ~/.ssh/id_rsa core@maroonedpods-group-test-cp-1`
- **Secret `worker-ignition-full`**: ~625KB Ignition config with encapsulated MC, OVS disabled, SSH key, resolv-prepender bypass, create-node-env service. Saved at `/tmp/worker-ignition-no-ovs.json`

### Manual fixes applied on live VM (need to be baked into Ignition):
1. Created `/etc/kubernetes/node.env` with `KUBELET_NODE_NAME` and `KUBELET_NODE_IP`
2. Copied kubelet-ca.crt from `kube-apiserver-to-kubelet-client-ca` configmap
3. Replaced bootstrap kubeconfig CA with full API server CA bundle from `openshift-config-managed/kube-apiserver-server-ca`
4. Deleted `/var/lib/kubelet/kubeconfig` and `/var/lib/kubelet/pki/` to force re-bootstrap

### Ignition config modifications (in `/tmp/worker-ignition-no-ovs.json`):
1. Encapsulated MachineConfig at `/etc/ignition-machine-config-encapsulated.json` (triggers MCD rebase)
2. OVS/OVN units disabled+masked (10 units)
3. OVS-related files removed (4 files)
4. Kubelet's `10-mco-on-prem-wait-resolv.conf` dropin removed
5. `resolv-prepender-bypass.service` added (creates `/run/resolv-prepender-kni-conf-done`)
6. `create-node-env.service` added (creates `/etc/kubernetes/node.env`)
7. SSH key added to both Ignition and encapsulated MC core user
8. `nodeip-configuration.service`, `openstack-hostname.service`, `openstack-kubelet-nodename.service` masked

### Key Image References
- **KubeVirt RHCOS containerDisk** (correct): `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:024cb7de8d9e21ef7fc2bcb90f8dd77e7f742d937822c95e4ec72b9ab48fea7d`
- **RHCOS machine-os-content** (for MCD rebase, osImageURL): `quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:759db300fd0e0258b2179aa48de5575baa2e33e6d0b6efebdf36cde5647baa40`

### Important CA bundles
- **API server serving CA** (for bootstrap kubeconfig): `oc get configmap -n openshift-config-managed kube-apiserver-server-ca -o jsonpath='{.data.ca-bundle\.crt}'` — 4 certs, includes `kube-apiserver-lb-signer`
- **Kubelet client CA** (for kubelet-ca.crt): `oc get configmap -n openshift-kube-apiserver-operator kube-apiserver-to-kubelet-client-ca -o jsonpath='{.data.ca-bundle\.crt}'`

## Compilation Status

✅ API types compile: `go build ./staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1/...`  
✅ Controller compiles: `go build ./pkg/maroonedpods-controller/maroonedpods-gate-controller/...`
✅ Images built and pushed: `quay.io/vladikr/maroonedpods-*:group-mode-ignition`

## Next Steps After Compact

### 1. Fix CNI (Issue 16) — make node Ready
The node is NotReady because no CNI config exists at `/etc/kubernetes/cni/net.d/`. Options:
- **Option A**: Install a simple bridge CNI plugin that uses the masquerade NAT interface. Write a minimal CNI config to `/etc/kubernetes/cni/net.d/10-bridge.conflist`.
- **Option B**: Re-evaluate whether OVN can work inside the VM with masquerade NAT. This is what HyperShift does — OVN runs inside the hosted cluster VMs. But in our case we're joining the SAME cluster, so we'd have nested OVN.
- **Option C**: Check if the existing cluster's OVN daemon pod will get scheduled to this node and configure CNI automatically once the node is Ready (chicken-and-egg: node NotReady because no CNI, no CNI because no OVN pod, no OVN pod because node NotReady).

### 2. Bake manual fixes into Ignition
Once the approach is proven, bake issues 13-15 fixes into the Ignition config so the full boot cycle works without manual intervention:
- Add kubelet-ca.crt file
- Use correct CA bundle (kube-apiserver-server-ca, not root-ca alone) in bootstrap kubeconfig
- create-node-env.service (already in Ignition)
- resolv-prepender-bypass.service (already in Ignition)

### 3. Verify pod scheduling
Once node is Ready, verify that the gated pods (`cp-etcd`, `cp-apiserver`) get ungated and scheduled to the VM node.

## Test Pod YAML
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: cp-etcd
  namespace: test-group-mode
  labels:
    maroonedpods.io/maroon: "true"
    maroonedpods.io/group: "test-cp-1"
spec:
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
---
apiVersion: v1
kind: Pod
metadata:
  name: cp-apiserver
  namespace: test-group-mode
  labels:
    maroonedpods.io/maroon: "true"
    maroonedpods.io/group: "test-cp-1"
spec:
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
```
