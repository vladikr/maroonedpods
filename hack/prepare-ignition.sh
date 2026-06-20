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
    'wait-for-primary-ip.service'
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

# Add the encapsulated MC as a file
encapsulated_mc_json = json.dumps(encapsulated_mc)
add_or_replace_file('/etc/ignition-machine-config-encapsulated.json', encapsulated_mc_json, 0o600)

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
