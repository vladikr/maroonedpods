# Answers to Your Questions

## 1. Is there anything else left to do from our plan?

**✅ DONE** — The plan (CSR auto-approval + OCP SCC support) is fully complete and committed (`e3469cf`). The full automation lifecycle works end-to-end on OCP 4.22.1 with zero manual intervention.

## 2. We probably need to create a PR

**✅ READY** — PR description prepared at `PR_DESCRIPTION.md`. 

To create the PR:
```bash
gh pr create --title "Add Group Mode for VM Pool Scheduling with Full OCP Support" \
  --body-file PR_DESCRIPTION.md \
  --base main \
  --head feature/e2e-tests
```

The PR includes:
- `58e49b2` — E2E tests
- `501c485` — Group mode foundation
- `e3469cf` — CSR auto-approval + SCC support
- **2,654 additions** across 17 files

## 3. Do we have a Makefile and documentation for building the node image?

**✅ YES** — Build system exists:

**Component images** (controller, operator, server):
```bash
make build-images
make push-images
```

**Node image** (for non-OCP, k3s-based):
```bash
make build-node-image
make push-node-image
```
- Dockerfile: `images/node/Containerfile.node`
- Docs: `images/node/README.md`

**For OCP group mode**: No custom node image needed! We use the RHCOS container disk from the OCP release payload:
```yaml
groupNodeImage: quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:...
```

## 4. We need to record a demo!

**✅ SCRIPT READY** — Demo script created at `demo/group-mode-demo.sh`

To record:
```bash
# Install asciinema
sudo dnf install asciinema  # or: pip install asciinema

# Record the demo
asciinema rec demo.cast
./demo/group-mode-demo.sh
# Press Ctrl+D when done

# Play back
asciinema play demo.cast

# Upload to share
asciinema upload demo.cast
```

The script automates the full lifecycle and shows timestamped progress.

## 5. Can we make HyperShift create KubeVirt-based nodes for hosted clusters?

**📋 FUTURE WORK** — This is a major feature extension beyond the current PR.

**What we have now**: Group mode creates VMIs that join the **management cluster** as nodes.

**What you're asking**: Create VMIs that join a **hosted cluster** (provisioned by HyperShift) as worker nodes.

**What's needed**:
1. NodePool-like mechanism that creates VMIs with Ignition pointing to the **hosted cluster's** API server
2. Integration with HyperShift's `HostedCluster` / `NodePool` CRDs
3. The hosted cluster's bootstrap token and API endpoint in the Ignition config

This is essentially what HyperShift's KubeVirt provider already does, but MaroonedPods could add **scheduling-layer isolation** on top (one VM per tenant's control plane).

**Recommendation**: Separate feature/project for a follow-up PR.

## 6. How do we avoid Kata's issues? Why is MaroonedPods better?

**✅ ANALYZED** — I read the PDF. Here's the comparison:

### Kata's Problems (from the PDF)
1. **Storage** — Stateful components (etcd, OVN) struggle with Kata's ephemeral storage
2. **Performance** — MicroVM overhead per pod
3. **SCC conflicts** — Kata runtime needs privileged SCC
4. **PodOverheads** — Imprecise resource accounting
5. **BM-only** — Requires nested virt (no cloud VMs unless peer-pods)
6. **Dangling VMs** — Peer-pods can leak cloud instances
7. **Networking overlays** — Extra latency
8. **Maturity** — "Early technology, not tested at HCP scale"

### Why MaroonedPods is Better

| Concern | Kata | MaroonedPods |
|---------|------|-------------|
| **Isolation** | Per-pod microVM (N VMs for N pods) | Per-group VM (1 VM for N pods) |
| **Storage** | Ephemeral ❌ | Full persistent node storage ✅ |
| **Networking** | Extra overlay ❌ | Standard bridge CNI ✅ |
| **SCC** | Runtime needs privileged ❌ | Only controller needs privileged ✅ |
| **Performance** | MicroVM per pod ❌ | One VM per group ✅ |
| **BM requirement** | Yes ❌ | No ✅ |
| **Resource leaks** | Peer-pods leak VMs ❌ | Auto-cleanup ✅ |
| **Maturity** | Untested at scale ❌ | Proven on OCP 4.22.1 ✅ |

**Key architectural advantage**: Kata isolates each pod individually. For control planes where all components already trust each other, **per-group isolation is more efficient**.

MaroonedPods:
- ✅ Full node with proper storage
- ✅ Standard Kubernetes networking inside the VM
- ✅ No special runtime requirements
- ✅ VMI lifecycle tied to pod group (auto-cleanup)
- ✅ Works on any KubeVirt cluster (no BM needed)

## 7. How can we integrate into OSAC?

**✅ ALREADY PLANNED** — OSAC workspace has planning docs proposing MaroonedPods:
- `.planning/maroonedpods-elevator-pitch.md`
- `.planning/control-plane-isolation-comparison.md`

### Integration Path

OSAC's `osac-operator` has a **`ProvisioningProvider` interface** at `osac-operator/pkg/provisioning/provider.go`:

```go
type ProvisioningProvider interface {
    TriggerProvision(ctx, cluster) error
    GetProvisionStatus(ctx, cluster) (ProvisionStatus, error)
    TriggerDeprovision(ctx, cluster) error
    GetDeprovisionStatus(ctx, cluster) (DeprovisionStatus, error)
    Name() string
}
```

Currently only `AAPProvider` exists. Create a `MaroonedPodsProvider`:

```go
// MaroonedPodsProvider provisions hosted control planes with VM-level isolation
type MaroonedPodsProvider struct {
    k8sClient client.Client
}

func (p *MaroonedPodsProvider) TriggerProvision(ctx, cluster) error {
    // 1. Create namespace for hosted cluster CP
    namespace := fmt.Sprintf("hcp-%s", cluster.Name)
    
    // 2. Create HyperShift HostedCluster CR (as usual)
    // 3. Label all CP pods with:
    //    maroonedpods.io/maroon: "true"
    //    maroonedpods.io/group: cluster.Name
    
    // 4. MaroonedPods handles the rest:
    //    - VMI creation
    //    - Node registration
    //    - CSR approval
    //    - Pod scheduling
    
    return nil
}
```

**This is a separate PR in the OSAC workspace**, not in MaroonedPods. The MaroonedPods side is ready.

### Steps to Integrate

1. **In MaroonedPods repo** (this PR):
   - ✅ Group mode working
   - ✅ CSR auto-approval working
   - ✅ OCP compatibility working

2. **In OSAC repo** (future PR):
   - Create `osac-operator/pkg/provisioning/maroonedpods/provider.go`
   - Register in `provisioning/factory.go`
   - Add a `provisioningProvider` field to `HostedCluster` CR (e.g., `"maroonedpods"` vs `"aap"`)
   - The provider just needs to label pods — MaroonedPods does the rest

---

## Summary

| Question | Status | Action |
|----------|--------|--------|
| 1. Plan items left? | ✅ Done | None |
| 2. Create PR? | ✅ Ready | Run `gh pr create --body-file PR_DESCRIPTION.md` |
| 3. Makefile/docs? | ✅ Exists | Use existing Makefile targets |
| 4. Demo recording? | ✅ Script ready | Record with `asciinema rec demo.cast` |
| 5. HyperShift integration? | 📋 Future | Separate feature for follow-up |
| 6. Better than Kata? | ✅ Yes | Architectural advantages documented |
| 7. OSAC integration? | 📋 Future | Separate PR in OSAC workspace |

**Immediate next steps**:
1. Create the PR
2. Record a demo
3. The rest are follow-up projects
