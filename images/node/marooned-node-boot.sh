#!/bin/bash
#
# MaroonedPods Node Boot Script
# Reads configuration from cloud-init and starts k3s agent with pod-specific configuration

set -e

MAROONED_CONFIG="/etc/marooned/join-info.yaml"
LOG_PREFIX="[marooned-node-boot]"

log() {
    echo "$LOG_PREFIX $*"
}

error() {
    echo "$LOG_PREFIX ERROR: $*" >&2
    exit 1
}

# Wait for cloud-init to write configuration
wait_for_config() {
    local timeout=60
    local elapsed=0

    log "Waiting for configuration file $MAROONED_CONFIG..."

    while [ ! -f "$MAROONED_CONFIG" ]; do
        if [ $elapsed -ge $timeout ]; then
            error "Timeout waiting for $MAROONED_CONFIG"
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done

    log "Configuration file found"
}

# Parse YAML configuration (simple key:value parser)
parse_config() {
    if [ ! -f "$MAROONED_CONFIG" ]; then
        error "Configuration file not found: $MAROONED_CONFIG"
    fi

    # Read configuration values
    SERVER_URL=$(grep "^server_url:" "$MAROONED_CONFIG" | awk '{print $2}' | tr -d '"' | tr -d "'")
    TOKEN=$(grep "^token:" "$MAROONED_CONFIG" | awk '{print $2}' | tr -d '"' | tr -d "'")
    POD_UID=$(grep "^pod_uid:" "$MAROONED_CONFIG" | awk '{print $2}' | tr -d '"' | tr -d "'")
    TAINT_KEY=$(grep "^taint_key:" "$MAROONED_CONFIG" | awk '{print $2}' | tr -d '"' | tr -d "'")

    # Validate required fields
    if [ -z "$SERVER_URL" ]; then
        error "server_url not found in configuration"
    fi

    if [ -z "$TOKEN" ]; then
        error "token not found in configuration"
    fi

    if [ -z "$POD_UID" ]; then
        error "pod_uid not found in configuration"
    fi

    if [ -z "$TAINT_KEY" ]; then
        TAINT_KEY="maroonedpods.io"
        log "taint_key not specified, using default: $TAINT_KEY"
    fi

    log "Configuration parsed:"
    log "  server_url: $SERVER_URL"
    log "  pod_uid: $POD_UID"
    log "  taint_key: $TAINT_KEY"
}

# Start k3s agent with pod-specific configuration
start_k3s_agent() {
    log "Starting k3s agent..."

    # Build node labels
    NODE_LABELS="maroonedpods.io/pod-uid=$POD_UID"

    # Build node taints
    NODE_TAINTS="$TAINT_KEY/dedicated=$POD_UID:NoSchedule"

    # Create k3s agent config file
    mkdir -p /etc/rancher/k3s
    cat > /etc/rancher/k3s/config.yaml <<EOF
server: ${SERVER_URL}
token: ${TOKEN}
node-label:
  - ${NODE_LABELS}
node-taint:
  - ${NODE_TAINTS}
EOF

    log "Starting k3s-agent service with configuration:"
    log "  Labels: $NODE_LABELS"
    log "  Taints: $NODE_TAINTS"

    # Start k3s-agent service
    systemctl start k3s-agent.service

    # Wait for k3s-agent to become active
    local timeout=120
    local elapsed=0

    log "Waiting for k3s-agent to become active..."

    while ! systemctl is-active --quiet k3s-agent.service; do
        if [ $elapsed -ge $timeout ]; then
            error "Timeout waiting for k3s-agent service to start"
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done

    log "k3s-agent service is active"
}

# Check node readiness
check_readiness() {
    log "Checking node readiness..."

    # Wait for node to register
    local timeout=60
    local elapsed=0

    while [ $elapsed -lt $timeout ]; do
        if [ -f /var/lib/rancher/k3s/agent/kubelet.kubeconfig ]; then
            log "Node registered successfully"
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done

    log "Warning: Node may not be fully registered (timeout), but continuing..."
    return 0
}

# Main execution
main() {
    log "MaroonedPods Node Boot Starting..."

    wait_for_config
    parse_config
    start_k3s_agent
    check_readiness

    log "MaroonedPods Node Boot Complete"
}

main "$@"
