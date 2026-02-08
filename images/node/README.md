# MaroonedPods Node Image

This directory contains the bootable container image for MaroonedPods virtual nodes. The image uses [bootc](https://github.com/containers/bootc) technology to create a minimal, fast-booting Kubernetes node based on k3s agent.

## Overview

The MaroonedPods node image provides:
- **Minimal bootable OS** based on CentOS Stream 9 bootc
- **k3s agent** for lightweight Kubernetes node functionality
- **Fast boot time** with optional kernel boot support
- **Pod-specific configuration** via cloud-init integration
- **Low resource overhead** (~500m CPU, 512Mi memory for node components)

## Architecture

```
┌─────────────────────────────────────────────┐
│  MaroonedPods Node Container Image          │
│  (quay.io/vladikr/marooned-node)           │
├─────────────────────────────────────────────┤
│  ┌────────────────────────────────────┐    │
│  │  k3s agent                          │    │
│  │  - Joins cluster with pod-specific │    │
│  │    labels and taints                │    │
│  │  - Minimal node components          │    │
│  └────────────────────────────────────┘    │
│  ┌────────────────────────────────────┐    │
│  │  marooned-node-boot.service         │    │
│  │  - Reads cloud-init config          │    │
│  │  - Starts k3s with config           │    │
│  └────────────────────────────────────┘    │
│  ┌────────────────────────────────────┐    │
│  │  System Components                  │    │
│  │  - CNI plugins                      │    │
│  │  - iptables, socat, conntrack       │    │
│  │  - containerd, cri-tools            │    │
│  └────────────────────────────────────┘    │
└─────────────────────────────────────────────┘
```

## Components

### 1. Base Image
- **bootc base**: `quay.io/centos-bootc/centos-bootc:stream9`
- Provides bootable container technology
- Supports both containerDisk and kernelBoot methods

### 2. k3s Agent
- **Version**: v1.28.5+k3s1 (configurable)
- **Mode**: Agent-only (no control plane)
- **Configuration**: Read from `/etc/marooned/join-info.yaml`

### 3. Boot Script (`marooned-node-boot.sh`)
Reads cloud-init configuration and:
- Parses `server_url`, `token`, `pod_uid`, `taint_key`
- Configures k3s with pod-specific labels: `maroonedpods.io/pod-uid=$POD_UID`
- Applies node taints: `$TAINT_KEY/dedicated=$POD_UID:NoSchedule`
- Starts k3s-agent service
- Waits for node registration

### 4. Systemd Service (`marooned-node-boot.service`)
- **Type**: oneshot
- **Runs**: After network-online.target
- **Action**: Executes boot script to configure and start k3s

### 5. Network Components
- **CNI Plugins**: v1.5.0 from containernetworking/plugins
- **Tools**: iptables, socat, conntrack-tools for k3s networking

## Prerequisites

### Build Requirements
- **podman** or **docker** (podman recommended for bootc images)
- **bootc** (optional, for advanced builds)
- **Linux** host (bootc requires Linux)

### Runtime Requirements (in KubeVirt)
- KubeVirt v0.59.0+ (for containerDisk support)
- KubeVirt v1.0.0+ (for kernelBoot support, optional)

## Building the Image

### Quick Build
```bash
make build-node-image
```

This builds the image as `quay.io/vladikr/marooned-node:latest` using podman.

### Custom Build
```bash
# Build with custom tag
podman build -t my-registry/marooned-node:v1.0 -f images/node/Containerfile.node images/node/

# Build with different k3s version
podman build \
  --build-arg K3S_VERSION=v1.29.0+k3s1 \
  -t quay.io/vladikr/marooned-node:k3s-v1.29 \
  -f images/node/Containerfile.node \
  images/node/
```

### Using bootc (Advanced)
```bash
# Build bootc image
bootc build \
  --type=oci \
  --output-dir=./output \
  images/node/Containerfile.node

# Convert to container image
podman load < output/image.tar
```

## Pushing the Image

```bash
# Push to default registry
make push-node-image

# Push to custom registry
podman tag quay.io/vladikr/marooned-node:latest my-registry/marooned-node:v1.0
podman push my-registry/marooned-node:v1.0
```

**Note**: You must be authenticated to the registry:
```bash
podman login quay.io
```

## Integration with MaroonedPodsConfig

The node image is configured via the `MaroonedPodsConfig` CRD:

```yaml
apiVersion: maroonedpods.io/v1alpha1
kind: MaroonedPodsConfig
metadata:
  name: default
spec:
  # Node image (required)
  nodeImage: quay.io/vladikr/marooned-node:latest

  # Optional: Enable kernel boot for faster startup
  enableKernelBoot: true

  # Optional: Kernel boot configuration
  kernelBootConfig:
    kernelPath: /vmlinuz
    initrdPath: /initrd.img
    kernelArgs: "console=ttyS0 root=/dev/vda rw"

  # Base VM resources (minimum for k3s agent)
  baseVMResources:
    cpu: 1
    memoryMi: 1024  # 1Gi

  # Overhead for node components
  resourceOverhead:
    cpu: 500m      # k3s agent overhead
    memory: 512Mi  # k3s agent overhead
```

## How It Works

### 1. VM Creation
When a pod with `maroonedpods.io/maroon=true` label is created:
1. Controller creates a VirtualMachineInstance using this image
2. VM boots with containerDisk (or kernelBoot if enabled)
3. Cloud-init writes `/etc/marooned/join-info.yaml` with:
   ```yaml
   server_url: https://kubernetes.default.svc:6443
   token: <kubeadm-token>
   pod_uid: <pod-uuid>
   taint_key: maroonedpods.io
   ```

### 2. Node Bootstrap
1. `marooned-node-boot.service` starts after network is online
2. Boot script reads join-info.yaml
3. k3s agent starts with pod-specific labels and taints
4. Node joins cluster and registers

### 3. Pod Scheduling
1. Controller removes scheduling gate from pod
2. Pod schedules only to this node (via nodeSelector + taint/toleration)
3. Pod runs in isolated VM environment

## Boot Methods

### containerDisk (Default)
- **Pros**: Works on all KubeVirt versions, no external storage
- **Cons**: Slower boot (~30-60s)
- **Use case**: Standard deployments

```yaml
volumes:
  - name: rootdisk
    containerDisk:
      image: quay.io/vladikr/marooned-node:latest
```

### kernelBoot (Optional, Faster)
- **Pros**: Faster boot (~10-20s), direct kernel loading
- **Cons**: Requires KubeVirt 1.0+, more complex setup
- **Use case**: Performance-sensitive deployments

```yaml
domain:
  firmware:
    kernelBoot:
      container:
        image: quay.io/vladikr/marooned-node:latest
        kernelPath: /vmlinuz
        initrdPath: /initrd.img
      kernelArgs: "console=ttyS0 root=/dev/vda rw"
volumes:
  - name: rootdisk
    containerDisk:
      image: quay.io/vladikr/marooned-node:latest
```

## Customization

### Adding Packages
Edit `Containerfile.node`:
```dockerfile
RUN dnf install -y \
    your-package \
    another-package
```

### Changing k3s Version
Edit `Containerfile.node`:
```dockerfile
RUN curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="v1.29.0+k3s1" INSTALL_K3S_EXEC="agent" sh -
```

### Custom CNI Plugins
Edit `Containerfile.node`:
```dockerfile
RUN curl -L https://github.com/containernetworking/plugins/releases/download/v1.6.0/cni-plugins-linux-amd64-v1.6.0.tgz \
    | tar -C /opt/cni/bin -xz
```

### Adding Init Scripts
1. Create your script in `images/node/`
2. Copy it in `Containerfile.node`:
   ```dockerfile
   COPY my-init.sh /usr/local/bin/my-init.sh
   RUN chmod +x /usr/local/bin/my-init.sh
   ```
3. Call it from `marooned-node-boot.sh` or create a systemd unit

## Troubleshooting

### Image Build Fails
```bash
# Check podman version
podman version

# Build with verbose output
podman build --log-level=debug -f images/node/Containerfile.node images/node/
```

### Boot Script Fails in VM
```bash
# In VM, check systemd logs
journalctl -u marooned-node-boot.service

# Check k3s-agent status
systemctl status k3s-agent

# Check configuration
cat /etc/marooned/join-info.yaml
cat /etc/rancher/k3s/config.yaml
```

### Node Not Joining Cluster
1. Verify `server_url` is reachable from VM network
2. Check `token` is valid
3. Verify network connectivity: `ping kubernetes.default.svc` (inside VM)
4. Check k3s logs: `journalctl -u k3s-agent -f`

## Performance Characteristics

### Boot Times
- **containerDisk**: ~30-60 seconds
- **kernelBoot**: ~10-20 seconds

### Resource Usage
- **CPU**: ~200-300m idle, ~500m under load
- **Memory**: ~400-512Mi

### Image Size
- **Compressed**: ~400-500MB
- **Uncompressed**: ~1.2-1.5GB

## References

- [bootc Project](https://github.com/containers/bootc)
- [k3s Documentation](https://docs.k3s.io)
- [KubeVirt containerDisk](https://kubevirt.io/user-guide/virtual_machines/disks_and_volumes/#containerdisk)
- [KubeVirt kernelBoot](https://kubevirt.io/user-guide/virtual_machines/boot_modes/#kernel-boot)
- [CNI Plugins](https://github.com/containernetworking/plugins)
