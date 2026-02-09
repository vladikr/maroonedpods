# MaroonedPods [WIP]

An operator that enables container workloads to be isolated in Virtual Machines using KubeVirt.

## 🎯 What is MaroonedPods?

MaroonedPods provides **VM-level isolation for Kubernetes Pods** without changing how you write or deploy them. Your Pods remain standard Kubernetes objects but run on dedicated, ephemeral virtual machine nodes.

### Key Features

- **🔒 True Isolation**: Each pod runs on its own dedicated VM node
- **🚀 Fast Boot**: 10-60s VM startup with bootc + k3s agent
- **💰 Resource Efficient**: Minimal overhead with k3s agent-only nodes
- **🎨 Kubernetes-Native**: Standard Pods, standard kubectl, standard APIs
- **🔧 Zero Code Changes**: Label your pods, that's it

## 🏗️ Architecture

```
┌─────────────────────────────────────────────────────────┐
│  Regular Kubernetes Pod                                  │
│  apiVersion: v1                                          │
│  kind: Pod                                               │
│  metadata:                                               │
│    labels:                                               │
│      maroonedpods.io/maroon: "true"  ← Just add this!   │
└─────────────────────────────────────────────────────────┘
                          ↓
┌─────────────────────────────────────────────────────────┐
│  MaroonedPods Controller                                 │
│  1. Adds scheduling gate to pod                          │
│  2. Creates dedicated VirtualMachineInstance             │
│  3. VM boots with bootc + k3s agent                      │
│  4. Node joins cluster with pod-specific taint           │
│  5. Removes gate → Pod schedules to dedicated node       │
└─────────────────────────────────────────────────────────┘
                          ↓
┌─────────────────────────────────────────────────────────┐
│  KubeVirt VirtualMachineInstance                        │
│  ┌──────────────────────────────────────────────────┐  │
│  │  k3s agent node (minimal Kubernetes node)        │  │
│  │  - Joins with pod-specific label & taint         │  │
│  │  - Only this pod can schedule here               │  │
│  │  ┌────────────────────────────────────────────┐ │  │
│  │  │  Your Pod (running isolated in VM)         │ │  │
│  │  └────────────────────────────────────────────┘ │  │
│  └──────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

## 🚀 Quick Start

### Prerequisites

- Kubernetes cluster (1.26+)
- KubeVirt installed (0.59.0+)
- `kubectl` configured

### 1. Install MaroonedPods

```bash
kubectl apply -f https://github.com/vladikr/maroonedpods/releases/latest/download/maroonedpods-operator.yaml
```

### 2. (Optional) Build Custom Node Image

The default node image is `quay.io/vladikr/marooned-node:latest`. To build your own:

```bash
# Build the bootc+k3s node image
make build-node-image

# Push to your registry
make push-node-image

# Or build with custom tag
podman build -t my-registry/marooned-node:v1.0 -f images/node/Containerfile.node images/node/
podman push my-registry/marooned-node:v1.0
```

See [images/node/README.md](images/node/README.md) for detailed build instructions.

### 3. Deploy a Marooned Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: isolated-nginx
  labels:
    maroonedpods.io/maroon: "true"  # This triggers VM isolation
spec:
  containers:
  - name: nginx
    image: nginx:latest
    ports:
    - containerPort: 80
```

```bash
kubectl apply -f isolated-nginx.yaml
```

### 4. Watch it Work

```bash
# Watch the pod get gated, VM created, node joined, and pod scheduled
kubectl get pods -w

# See the dedicated VM
kubectl get vmi

# See the dedicated node
kubectl get nodes | grep isolated-nginx
```

## ⚙️ Configuration

MaroonedPods can be configured via the `MaroonedPodsConfig` CRD (when available):

```yaml
apiVersion: maroonedpods.io/v1alpha1
kind: MaroonedPodsConfig
metadata:
  name: default
spec:
  # Node image for VMs (bootc + k3s agent)
  nodeImage: quay.io/vladikr/marooned-node:latest

  # Warm pool: pre-boot VMs for instant scheduling
  warmPoolSize: 3  # Keep 3 ready VMs

  # Base VM resources (minimum)
  baseVMResources:
    cpu: 1
    memoryMi: 1024  # 1Gi

  # Overhead for node components (k3s agent, kubelet, etc.)
  resourceOverhead:
    cpu: 500m
    memory: 512Mi

  # Optional: Enable faster boot with kernelBoot
  enableKernelBoot: true
  kernelBootConfig:
    kernelPath: /vmlinuz
    initrdPath: /initrd.img
    kernelArgs: "console=ttyS0 root=/dev/vda rw"

  # Taint key for node affinity
  nodeTaintKey: maroonedpods.io
```

## 🔧 How It Works

### 1. Pod Submission
When you create a pod with `maroonedpods.io/maroon: "true"` label:
- Admission webhook adds a **scheduling gate** (pod can't schedule yet)
- Webhook adds **nodeSelector** and **toleration** for dedicated node
- Pod enters **Pending** state

### 2. VM Creation
MaroonedPods controller:
- Detects the gated pod
- Creates a `VirtualMachineInstance` using bootc+k3s node image
- Generates cloud-init with k3s join configuration
- VM boots in ~10-60 seconds (depending on boot method)

### 3. Node Registration
Inside the VM:
- `marooned-node-boot.service` runs on startup
- Reads `/etc/marooned/join-info.yaml` (from cloud-init)
- Starts k3s agent with pod-specific labels and taints
- Node joins cluster with name matching pod name

### 4. Pod Scheduling
MaroonedPods controller:
- Detects node is ready
- **Removes scheduling gate**
- Pod schedules to the dedicated node (via nodeSelector + taint)
- Pod runs isolated in the VM

### 5. Cleanup
When the pod is deleted:
- Finalizer triggers cleanup
- VM is either:
  - Returned to warm pool (if enabled)
  - Deleted (if warm pool disabled or full)

## 📦 Node Image Details

The node image (`quay.io/vladikr/marooned-node:latest`) is built using:

- **Base**: CentOS Stream 9 bootc
- **Agent**: k3s v1.28.5+k3s1 (agent-only mode)
- **CNI**: CNI plugins v1.5.0
- **Tools**: iptables, socat, conntrack-tools, cri-tools
- **Size**: ~400-500MB compressed

### Boot Methods

| Method | Boot Time | KubeVirt Version | Use Case |
|--------|-----------|------------------|----------|
| **containerDisk** | 30-60s | 0.59.0+ | Standard, most compatible |
| **kernelBoot** | 10-20s | 1.0.0+ | Performance-critical workloads |

### Resource Requirements

| Component | CPU | Memory |
|-----------|-----|--------|
| k3s agent | 200-300m | 400-512Mi |
| **Overhead** | **500m** | **512Mi** |
| **Base VM** | **1 CPU** | **1Gi** |

## 🎯 Use Cases

### Security-Critical Workloads
Run untrusted code with VM-level isolation instead of just container isolation.

### Multi-Tenancy
Give each tenant their own dedicated VM nodes without managing separate clusters.

### Legacy Applications
Run apps that need kernel modules, privileged operations, or specific kernel versions.

### Compliance
Meet regulatory requirements that mandate VM-level isolation.

## 📚 Advanced Topics

### Warm Pool

Pre-boot VMs for instant pod scheduling:

```yaml
spec:
  warmPoolSize: 5  # Keep 5 ready VMs
```

- VMs boot in advance and wait in "available" state
- Pod claims a VM from pool instantly (~1-2s)
- When pod deleted, VM returns to pool
- Pool auto-scales based on configuration

### Dynamic Right-Sizing

VMs sized based on pod resource requests + overhead:

```yaml
spec:
  containers:
  - resources:
      requests:
        cpu: 2
        memory: 4Gi
```

VM will be: 2.5 CPU (2 + 0.5 overhead), 4.5Gi RAM (4Gi + 512Mi overhead)

### KernelBoot for Fast Startup

Enable direct kernel loading for faster boot:

```yaml
spec:
  enableKernelBoot: true
```

Reduces boot time from ~60s to ~15-20s.

## 🛠️ Development

### Build from Source

```bash
# Build controller binaries
make build

# Build node image
make build-node-image

# Build everything
make all
```

### Run Tests

MaroonedPods uses Ginkgo/Gomega for testing.

```bash
# Unit tests (pkg and cmd packages)
make test

# Functional/E2E tests (requires cluster)
# This will: start cluster, deploy MaroonedPods, run tests, tear down
make functest

# Or run tests manually step-by-step:
make cluster-up        # Start test cluster with KubeVirt
make cluster-sync      # Deploy MaroonedPods operator and CRDs
make build-functest    # Build test binary
make functest          # Run tests
make cluster-down      # Clean up
```

### Cluster Setup for Development

```bash
# Start kind cluster with KubeVirt
make cluster-up

# Deploy MaroonedPods operator, CRDs, and sample config
make cluster-sync

# Interact with the cluster
./kubevirtci/kubectl.sh get pods -A
./kubevirtci/kubectl.sh get maroonedpodsconfigs

# Run a test pod
cat <<EOF | ./kubevirtci/kubectl.sh apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: test-nginx
  labels:
    maroonedpods.io/maroon: "true"
spec:
  containers:
  - name: nginx
    image: nginx:latest
EOF

# Watch the pod get isolated in a VM
./kubevirtci/kubectl.sh get pods -w
./kubevirtci/kubectl.sh get vmi

# Cleanup cluster
make cluster-down
```

### Testing Environment Variables

You can customize the test environment with these variables:

- `KUBEVIRT_PROVIDER`: Kubernetes provider (default: `k8s-1.27`)
- `KUBEVIRT_RELEASE`: KubeVirt version (default: `latest_nightly`)
- `MAROONEDPODS_NAMESPACE`: Namespace for operator (default: `maroonedpods`)
- `DOCKER_PREFIX`: Container image prefix
- `DOCKER_TAG`: Container image tag

Example:
```bash
KUBEVIRT_PROVIDER=k8s-1.27 KUBEVIRT_RELEASE=v1.1.0 make cluster-up
```

## 🤝 Contributing

We welcome contributions! Areas we're actively working on:

- [ ] Production-ready bootstrap token management
- [ ] Enhanced metrics and observability
- [ ] Support for PersistentVolumes in VMs
- [ ] Multi-architecture support (ARM64)
- [ ] Integration tests with real workloads

## 📖 Documentation

- [Node Image Build Guide](images/node/README.md)
- [Architecture Deep Dive](docs/architecture.md) (TBD)
- [Configuration Reference](docs/configuration.md) (TBD)

## 📺 Demo

[Demo Video](https://private-user-images.githubusercontent.com/1035064/303094621-139f906f-f01c-4497-9a1e-4bb4215617ad.webm?jwt=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJnaXRodWIuY29tIiwiYXVkIjoicmF3LmdpdGh1YnVzZXJjb250ZW50LmNvbSIsImtleSI6ImtleTUiLCJleHAiOjE3MDczMzAyMjQsIm5iZiI6MTcwNzMyOTkyNCwicGF0aCI6Ii8xMDM1MDY0LzMwMzA5NDYyMS0xMzlmOTA2Zi1mMDFjLTQ0OTctOWExZS00YmI0MjE1NjE3YWQud2VibT9YLUFtei1BbGdvcml0aG09QVdTNC1ITUFDLVNIQTI1NiZYLUFtei1DcmVkZW50aWFsPUFLSUFWQ09EWUxTQTUzUFFLNFpBJTJGMjAyNDAyMDclMkZ1cy1lYXN0LTElMkZzMyUyRmF3czRfcmVxdWVzdCZYLUFtei1EYXRlPTIwMjQwMjA3VDE4MTg0NFomWC1BbXotRXhwaXJlcz0zMDAmWC1BbXotU2lnbmF0dXJlPTg1NjkwM2Y2NWQ1ZDYxMmEyNzU2MzczYTMxZmM2MzM4YTJhNWViMDY2NmUwNzlhOWVmZWIwZWYwMDIxZmFmZTEmWC1BbXotU2lnbmVkSGVhZGVycz1ob3N0JmFjdG9yX2lkPTAma2V5X2lkPTAmcmVwb19pZD0wIn0.XxqNcdyEmLAUjwhA9g9O0tTHCMBHW-qm2zsO_h6eWWk)

## 📄 License

Apache 2.0 - See [LICENSE](LICENSE) for details.

## 🙏 Acknowledgments

Built on top of:
- [KubeVirt](https://kubevirt.io) - Kubernetes Virtualization API
- [bootc](https://github.com/containers/bootc) - Bootable Container Images
- [k3s](https://k3s.io) - Lightweight Kubernetes
