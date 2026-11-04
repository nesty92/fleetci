#!/bin/sh
# Runs fc.sh in its own container on fleetingd's built image.
# Usage: hack/m1/snap/fc-run.sh [clones] [ci|ubuntu]
# ci: Firecracker's CI kernel. ubuntu: the image's own kernel (the one
# fleetingd boots in Cloud Hypervisor), uncompressed from its bzImage.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
DATA=${FLEETCI_M1_DATA:-$HOME/.local/share/fleetci-m1}
W=${FLEETCI_FC_DATA:-$HOME/.local/share/fleetci-fc}
CLONES=${1:-3}
FLAVOR=${2:-ci}
FC_VERSION=1.17.0
KERNEL=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155
mkdir -p "$W"

docker run --rm --name fleetci-fc \
  --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --cap-add SYS_ADMIN \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined --sysctl net.ipv4.ip_forward=1 \
  -e CLONES="$CLONES" -e FLAVOR="$FLAVOR" \
  -v "$DATA/vms:/vms:ro" -v "$W:/w" -v "$HERE/fc.sh:/fc.sh:ro" \
  --entrypoint sh fleetci-m1-host -c "
    set -e
    apt-get update -qq && apt-get install -y -qq --no-install-recommends jq bc procps e2fsprogs fdisk util-linux zstd file >/dev/null
    curl -fsSL https://github.com/firecracker-microvm/firecracker/releases/download/v$FC_VERSION/firecracker-v$FC_VERSION-x86_64.tgz | tar -xz -C /tmp
    install -m 755 /tmp/release-v$FC_VERSION-x86_64/firecracker-v$FC_VERSION-x86_64 /usr/local/bin/firecracker
    [ -f /w/vmlinux ] || curl -fsSL -o /w/vmlinux $KERNEL
    nft add table ip fcnat
    nft add chain ip fcnat post '{ type nat hook postrouting priority 100; }'
    nft add rule ip fcnat post ip saddr 172.16.200.0/30 oifname eth0 masquerade
    sh /fc.sh /vms/resolute-server-cloudimg-amd64_decompressed.img /w/vmlinux /w
  "
