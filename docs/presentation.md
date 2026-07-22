# MaroonedPods: Sovereign Tenant Clusters on KubeVirt VMs
## Architecture, Challenges, and Path Forward

---

## 1. What We Built

A fully virtualized sovereign tenant cluster using MaroonedPods + HyperShift + KubeVirt:

- **Layer 1:** Control plane (etcd, kapi, router, ignition-server) runs inside a KubeVirt VM on the management cluster
- **Layer 2:** Tenant worker VMs join the tenant cluster and run workloads
- **Result:** Complete isolation — every component runs in a VM

**Demo:** https://asciinema.org/a/7ULUQkcCrSR9no7l (11 min, zero manual intervention)

---

## 2. Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                     MANAGEMENT CLUSTER (OCP 5.0)                           │
│                                                                             │
│  ┌──────────────┐   ┌──────────────┐   ┌──────────────┐                    │
│  │  Master Node  │   │  Master Node  │   │  Master Node  │                    │
│  └──────────────┘   └──────────────┘   └──────────────┘                    │
│                                                                             │
│  ┌──────────────────────────────┐  ┌──────────────────────────────┐        │
│  │      Worker Node (fbhs6)     │  │      Worker Node (872hk)     │        │
│  │                              │  │                              │        │
│  │  ┌────────────────────────┐  │  │  ┌────────────────────────┐  │        │
│  │  │  CP VM (bridge binding)│  │  │  │  Worker VM (CAPK)      │  │        │
│  │  │  IP: 10.129.x.x       │  │  │  │  IP: 10.131.x.x       │  │        │
│  │  │                        │  │  │  │                        │  │        │
│  │  │  ┌──────────────────┐  │  │  │  │  ┌──────────────────┐  │  │        │
│  │  │  │  Bridge Network  │  │  │  │  │  │  Bridge Network  │  │  │        │
│  │  │  │  10.244.0.0/24   │  │  │  │  │  │  10.244.0.0/24   │  │  │        │
│  │  │  │                  │  │  │  │  │  │                  │  │  │        │
│  │  │  │  ┌────┐ ┌─────┐ │  │  │  │  │  │  ┌─────────┐    │  │  │        │
│  │  │  │  │etcd│ │kapi │ │  │  │  │  │  │  │ pod/hello│    │  │  │        │
│  │  │  │  │3/3 │ │4/4  │ │  │  │  │  │  │  │Succeeded│    │  │  │        │
│  │  │  │  └────┘ └─────┘ │  │  │  │  │  │  └─────────┘    │  │  │        │
│  │  │  │  ┌──────┐┌────┐ │  │  │  │  │  │                  │  │  │        │
│  │  │  │  │router││ign │ │  │  │  │  │  │  kubelet ──────────────────►    │
│  │  │  │  │      ││srv │ │  │  │  │  │  │       registers    │  │  │  │   │
│  │  │  │  └──────┘└────┘ │  │  │  │  │  │       with kapi   │  │  │  │   │
│  │  │  └──────────────────┘  │  │  │  └──────────────────┘  │  │        │
│  │  │                        │  │  │                        │  │        │
│  │  │  socat proxy           │  │  │                        │  │        │
│  │  │  :6443 → kapi          │  │  │                        │  │        │
│  │  │  :8443 → router        │  │  │                        │  │        │
│  │  │  :9443 → ign-proxy     │  │  │                        │  │        │
│  │  └────────────────────────┘  │  └────────────────────────┘  │        │
│  └──────────────────────────────┘  └──────────────────────────────┘        │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────┐        │
│  │  OCP Router (HAProxy)                                           │        │
│  │  Routes: kapi, ignition, konnectivity, oauth                    │        │
│  │  ──► Proxy EndpointSlices ──► CP VM socat ──► bridge pods       │        │
│  └─────────────────────────────────────────────────────────────────┘        │
│                                                                             │
│  ┌────────────────────┐  ┌─────────────────────┐  ┌──────────────────┐     │
│  │ MaroonedPods       │  │ HyperShift Operator  │  │ MetalLB          │     │
│  │ Controller+Webhook │  │ + CPO                │  │ (LoadBalancer)   │     │
│  └────────────────────┘  └─────────────────────┘  └──────────────────┘     │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 3. Communication Flows

```
┌─────────────────────────────────────────────────────────────────────┐
│                      TRAFFIC FLOWS                                   │
│                                                                       │
│  1. External → Tenant API (kubectl):                                 │
│     Client ──► OCP Route ──► HAProxy ──► Proxy EndpointSlice         │
│            ──► CP VM pod IP:6443 ──► socat ──► kapi bridge pod       │
│                                                                       │
│  2. Worker VM → Tenant API (kubelet registration):                   │
│     Worker VM ──► OCP Route ──► HAProxy ──► Proxy EndpointSlice      │
│              ──► CP VM pod IP:6443 ──► socat ──► kapi bridge pod     │
│                                                                       │
│  3. Worker VM → Ignition (bootstrap):                                │
│     Worker VM ──► OCP Route ──► HAProxy ──► Proxy EndpointSlice      │
│              ──► CP VM pod IP:9443 ──► socat ──► ign-proxy bridge    │
│                                                                       │
│  4. CP pods → Cluster Services (DNS, API):                           │
│     bridge pod ──► DNAT iptables ──► ClusterIP ──► endpoint pod      │
│     (inside VM)   (same-node preferred for DNS)                      │
│                                                                       │
│  5. MaroonedPods Controller ──► K8s API:                             │
│     Creates VMI, EndpointSlices, NetworkPolicies, RBAC, NADs         │
│     Patches IngressController, node annotations (MCD fix)            │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 4. What We Had To Do (Honest Assessment)

### Fundamental Challenge
KubeVirt VMs with **bridge binding** get the pod's IP directly, making the VM routable on the cluster network. But pods running INSIDE the VM use a **separate bridge CNI** (10.244.0.0/24) that is invisible to the management cluster's OVN network. This creates a connectivity gap.

### Workarounds We Built

| Workaround | Why Needed | Impact |
|---|---|---|
| **socat reverse proxy** (auto-proxy systemd service) | Bridge pod IPs unreachable from outside the VM | Adds latency, single point of failure per port |
| **Proxy EndpointSlices** | OCP router needs routable endpoints for tenant Routes | Controller must continuously manage 5 EndpointSlices |
| **Mask 16 OVS/OVN services** | RHCOS boots full OVN stack that creates br-ex drop rules | Disables host networking inside VM — must use bridge CNI instead |
| **DNAT sync service** | Bridge pods can't reach ClusterIP services natively | iptables DNAT rules updated every 30s, fragile for large service counts |
| **NetworkPolicy overrides** | HyperShift creates deny-by-default policies that block VM traffic | Must create allow-all ingress+egress before node registration |
| **IngressController routeSelector removal** | HyperShift excludes its Routes from default OCP router | Management-cluster-wide side effect |
| **MCD annotation patching** | Machine Config Daemon keeps cordoning the virtual node | Fights the platform's own management system |
| **Gzip-compressed encapsulated MC** | KubeVirt config drive has 1MB ISO limit | Compression workaround for a platform limitation |
| **CIP policy.json patching** | OCP 5.0 signature verification blocks tenant release images | Per-node manual fix, not automatable in controller |
| **CNI guard service** | Multus CNI overrides our bridge CNI config | Daemon that watches and removes Multus config |

### What Works Well

| Component | Status |
|---|---|
| Bridge binding + MAC annotation | Stable, VM gets routable pod IP |
| HyperShift CPO on OCP 4.20 | Stable (OCPBUGS-56966 fixed), no cycling |
| Auto-proxy systemd service | Reliable, handles pod IP changes |
| Worker VM via NodePool + MachineConfig | Clean, uses standard HyperShift flow |
| CSR auto-approval | Automated in controller |
| HAProxy health checks | Correctly marks bridge endpoints DOWN |

---

## 5. The Real Problem: Bridge Pod Reachability

```
THE CORE ISSUE:

  Management Cluster OVN Network (10.128.0.0/14)
  ┌─────────────────────────────────────────┐
  │  Pods, Services, Routes — all routable  │
  │                                         │
  │  CP VM pod: 10.129.x.x  ← reachable    │
  └──────────────┬──────────────────────────┘
                 │
                 │  ← This boundary is the problem
                 │
  ┌──────────────▼──────────────────────────┐
  │  VM Internal Bridge Network             │
  │  10.244.0.0/24  ← NOT reachable        │
  │                                         │
  │  kapi:     10.244.0.29:6443             │
  │  router:   10.244.0.48:8443             │
  │  ign-proxy: 10.244.0.30:8443            │
  │  etcd:     10.244.0.12:2379             │
  └─────────────────────────────────────────┘

  Current fix:  socat on VM pod IP → bridge pod
  Proper fix:   Make 10.244.0.0/24 routable
```

---

## 6. Path Forward: Eliminating the Socat Hack

### Option A: VXLAN Tunnel (Near-term, works today)

```
  Management Cluster Node                CP VM
  ┌─────────────────────┐               ┌─────────────────────┐
  │                     │               │                     │
  │  vxlan100           │◄─── VXLAN ───►│  vxlan100           │
  │  10.244.0.254/24    │   (UDP 4789)  │  10.244.0.1/24      │
  │                     │               │  (bridge gateway)   │
  │  ip route add       │               │                     │
  │  10.244.0.0/24      │               │  kapi: 10.244.0.29  │
  │  dev vxlan100       │               │  etcd: 10.244.0.12  │
  └─────────────────────┘               └─────────────────────┘

  Result: Bridge pods directly reachable from management cluster
  Eliminates: socat proxy, proxy EndpointSlices, DNAT sync
```

- Add VXLAN interface in ignition (systemd service)
- One VXLAN tunnel per CP VM
- Simple, no external dependencies
- Could be implemented in 1-2 days

### Option B: BGP-EVPN via OVN-K Route Advertisements (Long-term, proper)

```
  Management Cluster                      CP VM
  ┌──────────────────────────┐           ┌──────────────────┐
  │  OVN-K + FRR-k8s         │           │  FRR (BGP peer)  │
  │                          │           │                  │
  │  RouteAdvertisement:     │◄── BGP ──►│  Advertises:     │
  │  import 10.244.0.0/24   │           │  10.244.0.0/24   │
  │  via VM pod IP           │           │  via self        │
  │                          │           │                  │
  │  All nodes learn route   │           │  EVPN VXLAN:     │
  │  automatically           │           │  L2 stretch       │
  └──────────────────────────┘           └──────────────────┘

  Result: Native OVN-K routing to bridge pods
  Eliminates: ALL proxy workarounds
  Enables: Multi-CP-VM scaling, live migration
```

- Uses OVN-K's [RouteAdvertisements](https://ovn-kubernetes.io/features/bgp-integration/route-advertisements/) + [FRR-k8s](https://github.com/metallb/frr-k8s)
- [OKEP-5088 (EVPN)](https://ovn-kubernetes.io/okeps/okep-5088-evpn/) could stretch L2 segment
- Standard DC networking — BGP+EVPN is the modern fabric
- More complex but production-grade and scalable

### Option C: OVN-K UDN as Primary Network Inside VM (Aspirational)

Replace bridge CNI entirely — make OVN-K manage the VM's internal pod network as a UserDefinedNetwork. Pods inside the VM would get OVN-managed IPs that are natively routable. This would require OVN-K to support nested overlays or a "network extension" model.

---

## 7. Recommendation

| Phase | Approach | Timeline |
|---|---|---|
| **Now** | Current socat proxy (automated, working) | Done |
| **Next** | VXLAN tunnel (eliminate socat, simple) | 1-2 weeks |
| **Future** | BGP-EVPN via OVN-K RouteAdvertisements | Depends on OVN-K EVPN maturity |

### Immediate Next Steps
1. ~~Automate all manual hacks~~ ✅ Done
2. ~~Create demo~~ ✅ Done  
3. Prototype VXLAN tunnel in ignition
4. Evaluate FRR-k8s deployment on management cluster
5. Engage OVN-K team about nested VM use case for RouteAdvertisements/EVPN

---

## 8. Key Metrics

| Metric | Value |
|---|---|
| CP VM boot to CP Ready | ~10 minutes |
| Worker VM boot to Node Ready | ~10 minutes |
| Total HC creation to pod running | ~20 minutes |
| CP pods running | 40+ |
| Manual steps (with automation) | 0 (CIP fix is cluster-wide, one-time) |
| Stability (OCP 4.20 CPO) | kapi unchanged 20+ minutes (vs 10-min cycling on 4.17) |
