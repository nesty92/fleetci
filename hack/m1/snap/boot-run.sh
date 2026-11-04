#!/bin/sh
# Runs boot.sh: cold boots on Cloud Hypervisor and Firecracker, same image.
# Usage: hack/m1/snap/boot-run.sh [runs]. Needs fleetingd's built image
# (hack/m1) and the root partition fc-run.sh extracts.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
DATA=${FLEETCI_M1_DATA:-$HOME/.local/share/fleetci-m1}
W=${FLEETCI_FC_DATA:-$HOME/.local/share/fleetci-fc}
FC_VERSION=1.17.0
[ -f "$W/root.base.ext4" ] || { echo "run fc-run.sh once first" >&2; exit 1; }

docker run --rm --name fleetci-boot \
  --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --cap-add SYS_ADMIN \
  --security-opt seccomp=unconfined --sysctl net.ipv4.ip_forward=1 \
  -e RUNS="${1:-3}" -e CH_RUNS="${CH_RUNS:-}" -e FC_ARGS="${FC_ARGS:-}" \
  -v "$DATA/vms:/vms:ro" -v "$W:/w" -v "$HERE/boot.sh:/boot.sh:ro" \
  --entrypoint sh fleetci-m1-host -c "
    set -e
    apt-get update -qq && apt-get install -y -qq --no-install-recommends cloud-image-utils jq bc procps e2fsprogs zstd >/dev/null
    curl -fsSL https://github.com/firecracker-microvm/firecracker/releases/download/v$FC_VERSION/firecracker-v$FC_VERSION-x86_64.tgz | tar -xz -C /tmp
    install -m 755 /tmp/release-v$FC_VERSION-x86_64/firecracker-v$FC_VERSION-x86_64 /usr/local/bin/firecracker
    sh /boot.sh /vms/resolute-server-cloudimg-amd64_decompressed.img /w
  "
