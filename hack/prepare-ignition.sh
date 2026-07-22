#!/bin/bash
set -euo pipefail

# prepare-ignition.sh
# Prepares a complete Ignition config for MaroonedPods group mode VMs on OpenShift
#
# Usage:
#   KUBECONFIG=path/to/kubeconfig RENDERED_MC=rendered-worker-XXX ./prepare-ignition.sh
#
# Environment variables:
#   KUBECONFIG       - Path to cluster kubeconfig (required)
#   RENDERED_MC      - Name of the rendered worker MachineConfig (required)
#   OUTPUT           - Output path for Ignition JSON (default: /tmp/maroonedpods-worker-ignition.json)
#   SSH_KEY          - Path to SSH public key for debugging (optional)

KUBECONFIG="${KUBECONFIG:-}"
RENDERED_MC="${RENDERED_MC:-}"
OUTPUT="${OUTPUT:-/tmp/maroonedpods-worker-ignition.json}"
SSH_KEY="${SSH_KEY:-}"

if [[ -z "$KUBECONFIG" ]]; then
    echo "Error: KUBECONFIG environment variable must be set"
    exit 1
fi

if [[ -z "$RENDERED_MC" ]]; then
    echo "Error: RENDERED_MC environment variable must be set (e.g., rendered-worker-aa2abdc99a88179c999475bb71c3d9e0)"
    exit 1
fi

echo "Preparing Ignition config for OpenShift MaroonedPods group mode..."
echo "  Rendered MachineConfig: $RENDERED_MC"
echo "  Output: $OUTPUT"

# Extract base Ignition config from rendered MachineConfig
echo "Extracting base Ignition config..."
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT
oc get machineconfig "$RENDERED_MC" -o jsonpath='{.spec.config}' > "$TMPDIR/base-ignition.json"

# Extract CA bundles
echo "Extracting CA bundles..."
oc get configmap -n openshift-kube-apiserver-operator kube-apiserver-to-kubelet-client-ca -o jsonpath='{.data.ca-bundle\.crt}' > "$TMPDIR/kubelet-ca.crt"
oc get configmap -n openshift-config-managed kube-apiserver-server-ca -o jsonpath='{.data.ca-bundle\.crt}' > "$TMPDIR/api-server-ca.crt"

# Get the machine-os-content image URL (for encapsulated MC)
echo "Getting machine-os-content image..."
MOS_IMAGE=$(oc get machineconfig "$RENDERED_MC" -o jsonpath='{.spec.osImageURL}')

# Get API server URL and bootstrap token for kubeconfig
echo "Getting API server URL..."
API_SERVER_URL=$(oc get infrastructure cluster -o jsonpath='{.status.apiServerURL}')
echo "  API Server: $API_SERVER_URL"

echo "Creating bootstrap token..."
BOOTSTRAP_TOKEN=$(oc create token node-bootstrapper -n openshift-machine-config-operator --duration=87600h)

# Optionally load SSH key
SSH_KEY_CONTENT=""
if [[ -n "$SSH_KEY" && -f "$SSH_KEY" ]]; then
    echo "Loading SSH key from $SSH_KEY..."
    SSH_KEY_CONTENT=$(cat "$SSH_KEY")
fi

# Process Ignition config with Python
echo "Processing Ignition config..."
export MOS_IMAGE RENDERED_MC SSH_KEY_CONTENT OUTPUT TMPDIR API_SERVER_URL BOOTSTRAP_TOKEN
python3 <<'PYTHON_SCRIPT'
import json
import base64
import os
import sys

# Load environment variables
tmpdir = os.environ['TMPDIR']
mos_image = os.environ['MOS_IMAGE']
rendered_mc = os.environ['RENDERED_MC']
ssh_key_content = os.environ.get('SSH_KEY_CONTENT', '')
output_path = os.environ['OUTPUT']
api_server_url = os.environ.get('API_SERVER_URL', '')
bootstrap_token = os.environ.get('BOOTSTRAP_TOKEN', '')

# Load data from temp files
with open(f'{tmpdir}/base-ignition.json') as f:
    ign = json.load(f)
with open(f'{tmpdir}/kubelet-ca.crt') as f:
    kubelet_ca = f.read()
with open(f'{tmpdir}/api-server-ca.crt') as f:
    api_server_ca = f.read()

# Ensure storage.files exists
if 'storage' not in ign:
    ign['storage'] = {}
if 'files' not in ign['storage']:
    ign['storage']['files'] = []

# Helper to add/replace file
def add_or_replace_file(path, contents, mode=0o644):
    # Remove existing file with same path
    ign['storage']['files'] = [f for f in ign['storage']['files'] if f.get('path') != path]
    # Add new file
    contents_b64 = base64.standard_b64encode(contents.encode()).decode()
    ign['storage']['files'].append({
        'path': path,
        'mode': mode,
        'contents': {
            'source': f'data:text/plain;charset=utf-8;base64,{contents_b64}'
        }
    })

# Helper to remove file
def remove_file(path):
    ign['storage']['files'] = [f for f in ign['storage']['files'] if f.get('path') != path]

# 1. Add kubelet-ca.crt
print("Adding /etc/kubernetes/kubelet-ca.crt...")
add_or_replace_file('/etc/kubernetes/kubelet-ca.crt', kubelet_ca, 0o644)

# 2. Create bootstrap kubeconfig
print("Creating bootstrap kubeconfig...")
if api_server_url and bootstrap_token:
    import yaml
    api_server_ca_b64 = base64.standard_b64encode(api_server_ca.encode()).decode()
    bootstrap_kubeconfig = {
        'apiVersion': 'v1',
        'kind': 'Config',
        'clusters': [{
            'name': 'cluster',
            'cluster': {
                'server': api_server_url,
                'certificate-authority-data': api_server_ca_b64,
            }
        }],
        'users': [{
            'name': 'kubelet',
            'user': {
                'token': bootstrap_token,
            }
        }],
        'contexts': [{
            'name': 'kubelet',
            'context': {
                'cluster': 'cluster',
                'user': 'kubelet',
            }
        }],
        'current-context': 'kubelet',
    }
    kubeconfig_yaml = yaml.dump(bootstrap_kubeconfig)
    add_or_replace_file('/etc/kubernetes/kubeconfig', kubeconfig_yaml, 0o600)
    print("  Bootstrap kubeconfig created with node-bootstrapper token")
else:
    print("  WARNING: API_SERVER_URL or BOOTSTRAP_TOKEN not set, skipping bootstrap kubeconfig")

# 3. Add bridge CNI config
print("Adding bridge CNI config...")
cni_config = {
    "cniVersion": "0.3.1",
    "name": "bridge-net",
    "plugins": [
        {
            "type": "bridge",
            "bridge": "cni0",
            "isGateway": True,
            "ipMasq": True,
            "ipam": {
                "type": "host-local",
                "ranges": [[{"subnet": "10.244.0.0/24"}]],
                "routes": [{"dst": "0.0.0.0/0"}]
            }
        },
        {
            "type": "portmap",
            "capabilities": {"portMappings": True}
        }
    ]
}
add_or_replace_file('/etc/kubernetes/cni/net.d/10-bridge.conflist', json.dumps(cni_config, indent=2), 0o644)

# 4. Add cni-symlinks.service
print("Adding cni-symlinks.service...")
if 'systemd' not in ign:
    ign['systemd'] = {}
if 'units' not in ign['systemd']:
    ign['systemd']['units'] = []

cni_symlinks_unit = {
    "name": "cni-symlinks.service",
    "enabled": True,
    "contents": """[Unit]
Description=Create CNI binary symlinks
Before=kubelet.service
After=machine-config-daemon-firstboot.service

[Service]
Type=oneshot
ExecStart=/usr/bin/mkdir -p /var/lib/cni/bin
ExecStart=/usr/bin/ln -sf /usr/libexec/cni/bridge /var/lib/cni/bin/bridge
ExecStart=/usr/bin/ln -sf /usr/libexec/cni/host-local /var/lib/cni/bin/host-local
ExecStart=/usr/bin/ln -sf /usr/libexec/cni/portmap /var/lib/cni/bin/portmap
ExecStart=/usr/bin/ln -sf /usr/libexec/cni/loopback /var/lib/cni/bin/loopback
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
"""
}
# Remove if exists
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'cni-symlinks.service']
ign['systemd']['units'].append(cni_symlinks_unit)

# 4b. Add cni-guard script and service (removes Multus CNI config that overrides our bridge CNI)
print("Adding cni-guard service...")
cni_guard_script = r"""#!/bin/bash
LOG="/var/log/cni-guard.log"
CNI_DIR="/etc/kubernetes/cni/net.d"
MULTUS="$CNI_DIR/00-multus.conf"
RESTARTED=false

echo "$(date): cni-guard started" >> "$LOG"

while true; do
    if [ -f "$MULTUS" ]; then
        rm -f "$MULTUS" "$CNI_DIR/whereabouts.d" 2>/dev/null
        echo "$(date): removed multus config" >> "$LOG"
        if [ "$RESTARTED" = "false" ]; then
            systemctl restart crio 2>/dev/null
            echo "$(date): restarted crio" >> "$LOG"
            RESTARTED=true
        fi
    fi
    modprobe br_netfilter 2>/dev/null
    sysctl -qw net.bridge.bridge-nf-call-iptables=1 net.ipv4.ip_forward=1 2>/dev/null
    sleep 5
done
"""
add_or_replace_file('/usr/local/bin/cni-guard.sh', cni_guard_script, 0o755)

cni_guard_unit = {
    "name": "cni-guard.service",
    "enabled": True,
    "contents": """[Unit]
Description=Guard bridge CNI - removes Multus config from CRI-O CNI dir
After=cri-o.service
Wants=cri-o.service

[Service]
Type=simple
Restart=always
RestartSec=5
ExecStart=/usr/local/bin/cni-guard.sh

[Install]
WantedBy=multi-user.target
"""
}
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'cni-guard.service']
ign['systemd']['units'].append(cni_guard_unit)

# 4c. Add dynamic DNAT sync service (routes ClusterIP services for bridge CNI pods)
print("Adding service-dnat-sync service...")
dnat_sync_script = r"""#!/bin/bash
CHAIN="SVC-DNAT"
LOG="/var/log/service-dnat-sync.log"
KC="/var/lib/kubelet/kubeconfig"

while [ ! -f "$KC" ]; do
    echo "$(date): waiting for $KC..." >> "$LOG"
    sleep 5
done

iptables -t nat -N "$CHAIN" 2>/dev/null || true
iptables -t nat -C PREROUTING -j "$CHAIN" 2>/dev/null || iptables -t nat -I PREROUTING -j "$CHAIN"
iptables -t nat -N SVC-DNS 2>/dev/null || true
iptables -t nat -C OUTPUT -j SVC-DNS 2>/dev/null || iptables -t nat -I OUTPUT -j SVC-DNS

TENANT_NS=$(hostname | sed 's/maroonedpods-group-//')
echo "$(date): service-dnat-sync started, tenant=$TENANT_NS" >> "$LOG"

while true; do
    kubectl --kubeconfig "$KC" get svc -n "$TENANT_NS" -o json > /tmp/svcs.json 2>/dev/null
    kubectl --kubeconfig "$KC" get svc dns-default -n openshift-dns -o json > /tmp/svcs_dns.json 2>/dev/null
    kubectl --kubeconfig "$KC" get endpoints -n "$TENANT_NS" -o json > /tmp/eps.json 2>/dev/null
    kubectl --kubeconfig "$KC" get endpoints dns-default -n openshift-dns -o json > /tmp/eps_dns.json 2>/dev/null

    python3 -c "
import json
svcs = json.load(open('/tmp/svcs.json'))
eps = json.load(open('/tmp/eps.json'))
try:
    dns_svc = json.load(open('/tmp/svcs_dns.json'))
    svcs['items'].append(dns_svc)
except: pass
try:
    dns_ep = json.load(open('/tmp/eps_dns.json'))
    eps['items'].append(dns_ep)
except: pass
json.dump(svcs, open('/tmp/svcs.json','w'))
json.dump(eps, open('/tmp/eps.json','w'))
" 2>/dev/null

    if [ ! -s /tmp/svcs.json ] || [ ! -s /tmp/eps.json ]; then
        sleep 10
        continue
    fi

    NEW_RULES=$(python3 -c "
import json, socket
svcs = json.load(open('/tmp/svcs.json'))
eps = json.load(open('/tmp/eps.json'))
import subprocess
try:
    out = subprocess.check_output(['ip','-4','-o','addr','show','enp1s0'], text=True)
    vm_ip = out.split('inet ')[1].split('/')[0]
    vm_subnet = '.'.join(vm_ip.split('.')[:2])
except:
    vm_subnet = ''
ep_map = {}
for ep in eps.get('items', []):
    ns = ep['metadata']['namespace']
    name = ep['metadata']['name']
    for subset in ep.get('subsets', []):
        for addr in subset.get('addresses', []):
            for port in subset.get('ports', []):
                ep_map.setdefault((ns,name),[]).append((addr['ip'], port['port'], port.get('protocol','TCP').lower(), port.get('name','')))
for svc in svcs.get('items', []):
    cip = svc['spec'].get('clusterIP','')
    if not cip or cip == 'None': continue
    ns = svc['metadata']['namespace']
    name = svc['metadata']['name']
    endpoints = ep_map.get((ns,name), [])
    if not endpoints: continue
    for port in svc['spec'].get('ports', []):
        proto = port.get('protocol','TCP').lower()
        sp = port['port']
        tp = port.get('targetPort', sp)
        pname = port.get('name', '')
        matching = []
        for eip, ep, eproto, ename in endpoints:
            if eproto != proto: continue
            if ep == sp or ep == tp or (isinstance(tp, str) and ename == tp) or (pname and ename == pname):
                matching.append((eip, ep))
        if not matching: continue
        local = [m for m in matching if m[0].startswith(vm_subnet)]
        eip, ep = (local or matching)[0]
        print(f'-d {cip}/32 -p {proto} --dport {sp} -j DNAT --to-destination {eip}:{ep}')
" 2>/dev/null)

    if [ -n "$NEW_RULES" ]; then
        iptables -t nat -F "$CHAIN" 2>/dev/null
        iptables -t nat -F SVC-DNS 2>/dev/null

        API_EP=$(kubectl --kubeconfig "$KC" get endpoints kubernetes -n default -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null)
        if [ -n "$API_EP" ]; then
            iptables -t nat -A "$CHAIN" -d 172.30.0.1/32 -p tcp --dport 443 -j DNAT --to-destination $API_EP:6443 2>/dev/null
        fi

        while IFS= read -r rule; do
            iptables -t nat -A "$CHAIN" $rule 2>/dev/null
            if echo "$rule" | grep -q "\-\-dport 53 "; then
                iptables -t nat -A SVC-DNS $rule 2>/dev/null
            fi
        done <<< "$NEW_RULES"
        RULE_COUNT=$(echo "$NEW_RULES" | wc -l)
        echo "$(date): synced $RULE_COUNT DNAT rules (+ API + DNS)" >> "$LOG"
    fi

    sleep 30
done
"""
add_or_replace_file('/usr/local/bin/service-dnat-sync.sh', dnat_sync_script, 0o755)

dnat_sync_unit = {
    "name": "service-dnat-sync.service",
    "enabled": True,
    "contents": """[Unit]
Description=Dynamic DNAT sync for Kubernetes services
After=network-online.target kubelet.service
Wants=kubelet.service

[Service]
Type=simple
Restart=always
RestartSec=10
ExecStart=/usr/local/bin/service-dnat-sync.sh

[Install]
WantedBy=multi-user.target
"""
}
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'service-dnat-sync.service']
ign['systemd']['units'].append(dnat_sync_unit)

# 4d. Add pod-network-nat service (MASQUERADE + FORWARD for bridge CNI pods)
print("Adding pod-network-nat service...")
pod_nat_unit = {
    "name": "pod-network-nat.service",
    "enabled": True,
    "contents": """[Unit]
Description=Enable NAT and forwarding for bridge CNI pods
After=network-online.target
Before=kubelet.service

[Service]
Type=oneshot
RemainAfterExit=true
ExecStart=/bin/bash -c 'iptables -P FORWARD ACCEPT; iptables -t nat -A POSTROUTING -s 10.244.0.0/24 ! -d 10.244.0.0/24 -j MASQUERADE; iptables -A FORWARD -i cni0 -j ACCEPT; iptables -A FORWARD -o cni0 -j ACCEPT'

[Install]
WantedBy=multi-user.target
"""
}
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'pod-network-nat.service']
ign['systemd']['units'].append(pod_nat_unit)

# 4e. Add data-disk-setup service (formats and mounts HPP storage disk)
print("Adding data-disk-setup service...")
data_disk_unit = {
    "name": "data-disk-setup.service",
    "enabled": True,
    "contents": """[Unit]
Description=Format data disk and mount HPP storage
After=local-fs.target
Before=kubelet.service crio.service

[Service]
Type=oneshot
RemainAfterExit=true
ExecStart=/bin/bash -c 'DEV=/dev/vdc; if [ ! -b $$DEV ]; then exit 0; fi; if ! blkid $$DEV | grep -q TYPE; then mkfs.ext4 -F $$DEV; fi; mkdir -p /var/hpp-csi-local-basic; mount $$DEV /var/hpp-csi-local-basic'

[Install]
WantedBy=multi-user.target
"""
}
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'data-disk-setup.service']
ign['systemd']['units'].append(data_disk_unit)

# 4f. Add crio-storage-setup service (symlinks container storage to data disk)
print("Adding crio-storage-setup.service...")
crio_storage_unit = {
    "name": "crio-storage-setup.service",
    "enabled": True,
    "contents": """[Unit]
Description=Redirect CRI-O container storage to data disk
After=data-disk-setup.service
Before=crio.service

[Service]
Type=oneshot
RemainAfterExit=true
ExecStart=/bin/bash -c '\
  DATA=/var/hpp-csi-local-basic; \
  STORE=/var/lib/containers/storage; \
  if [ ! -d $$DATA ]; then echo "Data disk not mounted"; exit 0; fi; \
  if [ -L $$STORE ]; then echo "Already symlinked"; exit 0; fi; \
  mkdir -p $$DATA/containers-storage; \
  if [ -d $$STORE ]; then rm -rf $$STORE; fi; \
  ln -sf $$DATA/containers-storage $$STORE; \
  semanage fcontext -a -t container_file_t "$$DATA/containers-storage(/.*)?" 2>/dev/null || true; \
  restorecon -R $$DATA/containers-storage; \
  semanage fcontext -a -t container_file_t "$$DATA/csi(/.*)?" 2>/dev/null || true; \
  restorecon -R $$DATA; \
  echo "CRI-O storage redirected to $$DATA/containers-storage"'

[Install]
WantedBy=multi-user.target
"""
}
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'crio-storage-setup.service']
ign['systemd']['units'].append(crio_storage_unit)

# 4g. Add debug-boot service (sets core password for console access)
print("Adding debug-boot service...")
debug_script = r"""#!/bin/bash
echo 'core:debug123' | chpasswd
echo "debug: password set" > /var/log/debug-boot.log
"""
add_or_replace_file('/usr/local/bin/debug-boot.sh', debug_script, 0o755)

debug_boot_unit = {
    "name": "debug-boot.service",
    "enabled": True,
    "contents": """[Unit]
Description=Debug boot diagnostics and set core password
After=network-online.target kubelet.service
Wants=kubelet.service

[Service]
Type=oneshot
RemainAfterExit=true
ExecStart=/usr/local/bin/debug-boot.sh

[Install]
WantedBy=multi-user.target
"""
}
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'debug-boot.service']
ign['systemd']['units'].append(debug_boot_unit)

# 4h. Add auto-proxy service (discovers and proxies HyperShift bridge pod services)
print("Adding auto-proxy service...")
auto_proxy_script = r"""#!/bin/bash
LOG="/var/log/auto-proxy.log"
KC="/var/lib/kubelet/kubeconfig"
TENANT_NS=$(hostname | sed 's/maroonedpods-group-//')

echo "$(date): auto-proxy starting, tenant=$TENANT_NS" >> "$LOG"

while [ ! -f "$KC" ]; do
    echo "$(date): waiting for $KC..." >> "$LOG"
    sleep 5
done

echo "$(date): kubeconfig found, starting proxy loop" >> "$LOG"

while true; do
    # --- kapi: app=kube-apiserver, listen 6443 -> pod 6443 ---
    KAPI_IP=$(kubectl --kubeconfig "$KC" get pod -n "$TENANT_NS" -l app=kube-apiserver -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
    KAPI_CACHED=$(cat /tmp/kapi_ip 2>/dev/null)
    if [ -n "$KAPI_IP" ] && [ "$KAPI_IP" != "$KAPI_CACHED" ]; then
        echo "$(date): kapi IP changed: $KAPI_CACHED -> $KAPI_IP" >> "$LOG"
        pkill -f 'socat TCP4-LISTEN:6443' 2>/dev/null || true
        setsid socat TCP4-LISTEN:6443,fork,reuseaddr TCP4:${KAPI_IP}:6443 &
        echo "$KAPI_IP" > /tmp/kapi_ip
        echo "$(date): kapi proxy started on :6443 -> $KAPI_IP:6443" >> "$LOG"
    fi
    if [ -n "$KAPI_IP" ] && ! pgrep -f 'socat TCP4-LISTEN:6443' > /dev/null 2>&1; then
        setsid socat TCP4-LISTEN:6443,fork,reuseaddr TCP4:${KAPI_IP}:6443 &
        echo "$(date): kapi proxy restarted on :6443 -> $KAPI_IP:6443" >> "$LOG"
    fi

    # --- ignition-server-proxy: app=ignition-server-proxy, listen 9443 -> pod 8443 ---
    IGN_IP=$(kubectl --kubeconfig "$KC" get pod -n "$TENANT_NS" -l app=ignition-server-proxy -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
    IGN_CACHED=$(cat /tmp/ignition-server-proxy_ip 2>/dev/null)
    if [ -n "$IGN_IP" ] && [ "$IGN_IP" != "$IGN_CACHED" ]; then
        echo "$(date): ignition-server-proxy IP changed: $IGN_CACHED -> $IGN_IP" >> "$LOG"
        pkill -f 'socat TCP4-LISTEN:9443' 2>/dev/null || true
        setsid socat TCP4-LISTEN:9443,fork,reuseaddr TCP4:${IGN_IP}:8443 &
        echo "$IGN_IP" > /tmp/ignition-server-proxy_ip
        echo "$(date): ignition-server-proxy proxy started on :9443 -> $IGN_IP:8443" >> "$LOG"
    fi
    if [ -n "$IGN_IP" ] && ! pgrep -f 'socat TCP4-LISTEN:9443' > /dev/null 2>&1; then
        setsid socat TCP4-LISTEN:9443,fork,reuseaddr TCP4:${IGN_IP}:8443 &
        echo "$(date): ignition-server-proxy proxy restarted on :9443 -> $IGN_IP:8443" >> "$LOG"
    fi

    # --- router: app=private-router, listen 8443 -> pod 8443 ---
    ROUTER_IP=$(kubectl --kubeconfig "$KC" get pod -n "$TENANT_NS" -l app=private-router -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
    ROUTER_CACHED=$(cat /tmp/router_ip 2>/dev/null)
    if [ -n "$ROUTER_IP" ] && [ "$ROUTER_IP" != "$ROUTER_CACHED" ]; then
        echo "$(date): router IP changed: $ROUTER_CACHED -> $ROUTER_IP" >> "$LOG"
        pkill -f 'socat TCP4-LISTEN:8443' 2>/dev/null || true
        setsid socat TCP4-LISTEN:8443,fork,reuseaddr TCP4:${ROUTER_IP}:8443 &
        echo "$ROUTER_IP" > /tmp/router_ip
        echo "$(date): router proxy started on :8443 -> $ROUTER_IP:8443" >> "$LOG"
    fi
    if [ -n "$ROUTER_IP" ] && ! pgrep -f 'socat TCP4-LISTEN:8443' > /dev/null 2>&1; then
        setsid socat TCP4-LISTEN:8443,fork,reuseaddr TCP4:${ROUTER_IP}:8443 &
        echo "$(date): router proxy restarted on :8443 -> $ROUTER_IP:8443" >> "$LOG"
    fi

    sleep 10
done
"""
add_or_replace_file('/usr/local/bin/auto-proxy.sh', auto_proxy_script, 0o755)

auto_proxy_unit = {
    "name": "auto-proxy.service",
    "enabled": True,
    "contents": """[Unit]
Description=Auto-proxy for HyperShift bridge pod services
After=kubelet.service
Wants=kubelet.service

[Service]
Type=simple
Restart=always
RestartSec=10
ExecStart=/usr/local/bin/auto-proxy.sh

[Install]
WantedBy=multi-user.target
"""
}
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'auto-proxy.service']
ign['systemd']['units'].append(auto_proxy_unit)

# 5. Disable/mask OVS units
print("Disabling OVS/OVN units...")
ovs_units = [
    'openvswitch.service',
    'ovs-configuration.service',
    'ovs-vswitchd.service',
    'ovsdb-server.service',
    'wait-for-br-ex-up.service',
    'wait-for-ipsec-connect.service',
    'ipsec.service',
    'nmstate-configuration.service',
    'nmstate.service',
    'wait-for-primary-ip.service',
    'configure-ovs.service',
    'mtu-migration.service',
    'ovn-controller.service',
    'ovnkube-node.service',
    'ovnkube-controller.service',
    'NetworkManager-wait-online.service'
]
for unit_name in ovs_units:
    # Remove existing unit if present
    ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != unit_name]
    # Add disabled+masked unit
    ign['systemd']['units'].append({
        'name': unit_name,
        'enabled': False,
        'mask': True
    })

# 6. Remove OVS-related files
print("Removing OVS-related files...")
ovs_files = [
    '/usr/local/bin/configure-ovs.sh',
    '/etc/NetworkManager/conf.d/99-sdn.conf',
    '/etc/systemd/system/ovs-vswitchd.service.d/10-mco-default-env.conf',
    '/etc/systemd/system/ovsdb-server.service.d/10-mco-default-env.conf'
]
for path in ovs_files:
    remove_file(path)

# 7. Add resolv-prepender-bypass.service (if not exists)
print("Adding resolv-prepender-bypass.service...")
resolv_bypass_unit = {
    "name": "resolv-prepender-bypass.service",
    "enabled": True,
    "contents": """[Unit]
Description=Bypass resolv-prepender check
Before=kubelet.service

[Service]
Type=oneshot
ExecStart=/usr/bin/touch /run/resolv-prepender-kni-conf-done
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
"""
}
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'resolv-prepender-bypass.service']
ign['systemd']['units'].append(resolv_bypass_unit)

# 8. Add create-node-env.service
print("Adding create-node-env.service...")
node_env_unit = {
    "name": "create-node-env.service",
    "enabled": True,
    "contents": """[Unit]
Description=Create node.env for kubelet
Before=kubelet.service
After=machine-config-daemon-firstboot.service

[Service]
Type=oneshot
ExecStart=/bin/bash -c 'echo "KUBELET_NODE_NAME=$(hostname)" > /etc/kubernetes/node.env && echo "KUBELET_NODE_IP=$(ip -4 addr show enp1s0 | grep -oP \"(?<=inet )\\S+\" | cut -d/ -f1)" >> /etc/kubernetes/node.env'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
"""
}
ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != 'create-node-env.service']
ign['systemd']['units'].append(node_env_unit)

# 9. Mask nodeip-configuration and openstack services
print("Masking nodeip-configuration and openstack services...")
masked_units = [
    'nodeip-configuration.service',
    'openstack-hostname.service',
    'openstack-kubelet-nodename.service'
]
for unit_name in masked_units:
    ign['systemd']['units'] = [u for u in ign['systemd']['units'] if u.get('name') != unit_name]
    ign['systemd']['units'].append({
        'name': unit_name,
        'enabled': False,
        'mask': True
    })

# 10. Remove kubelet's 10-mco-on-prem-wait-resolv.conf dropin
print("Removing kubelet's 10-mco-on-prem-wait-resolv.conf dropin...")
remove_file('/etc/systemd/system/kubelet.service.d/10-mco-on-prem-wait-resolv.conf')

# 11. Add SSH key if provided
if ssh_key_content:
    print("Adding SSH key...")
    # Add to Ignition passwd.users
    if 'passwd' not in ign:
        ign['passwd'] = {}
    if 'users' not in ign['passwd']:
        ign['passwd']['users'] = []

    # Find core user or create
    core_user = None
    for user in ign['passwd']['users']:
        if user.get('name') == 'core':
            core_user = user
            break

    if not core_user:
        core_user = {'name': 'core'}
        ign['passwd']['users'].append(core_user)

    if 'sshAuthorizedKeys' not in core_user:
        core_user['sshAuthorizedKeys'] = []
    if ssh_key_content not in core_user['sshAuthorizedKeys']:
        core_user['sshAuthorizedKeys'].append(ssh_key_content)

# 12. Add encapsulated MachineConfig
print("Adding encapsulated MachineConfig...")
# Need to get the full rendered MC and encapsulate it
import subprocess
mc_json = subprocess.check_output([
    'oc', 'get', 'machineconfig', rendered_mc, '-o', 'json'
], env=os.environ).decode()
mc = json.loads(mc_json)

# Build the encapsulated MC
encapsulated_mc = {
    'apiVersion': mc['apiVersion'],
    'kind': mc['kind'],
    'metadata': {
        'name': mc['metadata']['name'],
        'labels': mc['metadata'].get('labels', {})
    },
    'spec': mc['spec']
}

# Add SSH key to encapsulated MC as well
if ssh_key_content:
    if 'config' not in encapsulated_mc['spec']:
        encapsulated_mc['spec']['config'] = {}
    if 'passwd' not in encapsulated_mc['spec']['config']:
        encapsulated_mc['spec']['config']['passwd'] = {}
    if 'users' not in encapsulated_mc['spec']['config']['passwd']:
        encapsulated_mc['spec']['config']['passwd']['users'] = []

    core_user_mc = None
    for user in encapsulated_mc['spec']['config']['passwd']['users']:
        if user.get('name') == 'core':
            core_user_mc = user
            break

    if not core_user_mc:
        core_user_mc = {'name': 'core'}
        encapsulated_mc['spec']['config']['passwd']['users'].append(core_user_mc)

    if 'sshAuthorizedKeys' not in core_user_mc:
        core_user_mc['sshAuthorizedKeys'] = []
    if ssh_key_content not in core_user_mc['sshAuthorizedKeys']:
        core_user_mc['sshAuthorizedKeys'].append(ssh_key_content)

# Disable OVS in encapsulated MC as well
if 'config' in encapsulated_mc['spec']:
    if 'systemd' not in encapsulated_mc['spec']['config']:
        encapsulated_mc['spec']['config']['systemd'] = {}
    if 'units' not in encapsulated_mc['spec']['config']['systemd']:
        encapsulated_mc['spec']['config']['systemd']['units'] = []

    for unit_name in ovs_units:
        encapsulated_mc['spec']['config']['systemd']['units'] = [
            u for u in encapsulated_mc['spec']['config']['systemd']['units'] if u.get('name') != unit_name
        ]
        encapsulated_mc['spec']['config']['systemd']['units'].append({
            'name': unit_name,
            'enabled': False,
            'mask': True
        })

# Add the encapsulated MC as a gzip-compressed file (saves ~300KB in the ignition)
import gzip as gzip_mod
encapsulated_mc_json = json.dumps(encapsulated_mc).encode()
compressed = gzip_mod.compress(encapsulated_mc_json)
compressed_b64 = base64.standard_b64encode(compressed).decode()
ign['storage']['files'] = [f for f in ign['storage']['files'] if f.get('path') != '/etc/ignition-machine-config-encapsulated.json']
ign['storage']['files'].append({
    'path': '/etc/ignition-machine-config-encapsulated.json',
    'mode': 0o600,
    'contents': {
        'compression': 'gzip',
        'source': f'data:;base64,{compressed_b64}'
    }
})
print(f"  Encapsulated MC: {len(encapsulated_mc_json)} bytes -> {len(compressed)} bytes (gzip)")

# Write output
print(f"Writing Ignition config to {output_path}...")
with open(output_path, 'w') as f:
    json.dump(ign, f, indent=2)

print("Done!")
PYTHON_SCRIPT

echo "Ignition config created successfully: $OUTPUT"
echo ""
echo "To create the Secret on the cluster:"
echo "  oc delete secret -n maroonedpods worker-ignition-full 2>/dev/null || true"
echo "  oc create secret generic worker-ignition-full -n maroonedpods --from-file=userData=$OUTPUT"
