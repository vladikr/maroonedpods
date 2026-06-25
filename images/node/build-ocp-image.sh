#!/bin/bash
#
# build-ocp-image.sh
# Build the MaroonedPods OCP node containerDisk.
#
# Pipeline:
#   1. Build bootc container image (CentOS Stream 9 + CRI-O + kubelet)
#   2. Convert to qcow2 disk image via bootc-image-builder
#   3. Package qcow2 into a KubeVirt containerDisk
#   4. Push containerDisk to registry
#
# Prerequisites:
#   - podman login quay.io
#   - sudo access (bootc-image-builder needs --privileged)
#
# NOTE: Run as your regular user, NOT with sudo.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BOOTC_IMG="${BOOTC_IMG:-quay.io/vladikr/maroonedpods-ocp-node-bootc:dev}"
CONTAINERDISK_IMG="${CONTAINERDISK_IMG:-quay.io/vladikr/maroonedpods-ocp-node:dev}"
OUTPUT_DIR="${SCRIPT_DIR}/output-ocp"

echo "=== Build MaroonedPods OCP Node ContainerDisk ==="
echo "Bootc image:      $BOOTC_IMG"
echo "ContainerDisk:    $CONTAINERDISK_IMG"
echo "Output dir:       $OUTPUT_DIR"
echo ""

cd "$SCRIPT_DIR"

# --- Step 1: Build and push bootc container ---
echo "Step 1: Building bootc container image..."
podman build --no-cache --dns=8.8.8.8 -f Containerfile.ocp -t "$BOOTC_IMG" .
echo "Pushing bootc image (needed for step 2)..."
podman push "$BOOTC_IMG"
echo ""

# --- Step 2: Convert to qcow2 ---
echo "Step 2: Converting bootc image to qcow2 via bootc-image-builder..."
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

cat > "$OUTPUT_DIR/config.toml" <<'TOML'
[[customizations.user]]
name = "root"
password = "debug123"
TOML

sudo podman pull "$BOOTC_IMG"
sudo podman run \
    --rm \
    --privileged \
    --pull=newer \
    --security-opt label=type:unconfined_t \
    -v "$OUTPUT_DIR":/output \
    -v /var/lib/containers/storage:/var/lib/containers/storage \
    quay.io/centos-bootc/bootc-image-builder:latest \
    --type qcow2 \
    --local \
    --config /output/config.toml \
    "$BOOTC_IMG"
echo ""

QCOW2_PATH="$OUTPUT_DIR/qcow2/disk.qcow2"
if [ ! -f "$QCOW2_PATH" ]; then
    echo "ERROR: qcow2 not found at $QCOW2_PATH"
    exit 1
fi
echo "qcow2 image: $(du -h "$QCOW2_PATH" | cut -f1)"
echo ""

# --- Step 3: Build containerDisk ---
echo "Step 3: Building KubeVirt containerDisk..."
cat > "$OUTPUT_DIR/Containerfile.containerdisk" <<'EOF'
FROM scratch
ADD --chown=107:107 qcow2/disk.qcow2 /disk/
EOF

podman build -f "$OUTPUT_DIR/Containerfile.containerdisk" -t "$CONTAINERDISK_IMG" "$OUTPUT_DIR"
echo ""

# --- Step 4: Push ---
echo "Step 4: Pushing containerDisk..."
podman push "$CONTAINERDISK_IMG"
echo ""

echo "=== ContainerDisk build complete ==="
echo "Image: $CONTAINERDISK_IMG"
