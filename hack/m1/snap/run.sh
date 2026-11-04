#!/bin/sh
# Runs snap.sh in its own container next to fleetci-m1, on fleetingd's built
# image. Usage: hack/m1/snap/run.sh [copyonwrite|ondemand|copy] [clones]
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
DATA=${FLEETCI_M1_DATA:-$HOME/.local/share/fleetci-m1}
W=${FLEETCI_SNAP_DATA:-$HOME/.local/share/fleetci-snap}
MODE=${1:-copyonwrite}
CLONES=${2:-3}
CH_VERSION=54.0
mkdir -p "$W"

docker run --rm --name fleetci-snap \
  --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --cap-add SYS_ADMIN --cap-add SYS_PTRACE \
  --security-opt seccomp=unconfined --sysctl net.ipv4.ip_forward=1 \
  -e MODE="$MODE" -e CLONES="$CLONES" \
  -v "$DATA/vms:/vms:ro" -v "$W:/w" -v "$HERE/snap.sh:/snap.sh:ro" \
  --entrypoint sh fleetci-m1-host -c "
    set -e
    apt-get update -qq && apt-get install -y -qq --no-install-recommends cloud-image-utils jq bc procps >/dev/null
    curl -fsSL -o /usr/local/bin/ch-remote https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v$CH_VERSION/ch-remote-static
    chmod 755 /usr/local/bin/ch-remote
    nft add table ip snapnat
    nft add chain ip snapnat post '{ type nat hook postrouting priority 100; }'
    nft add rule ip snapnat post ip saddr 172.16.200.0/30 oifname eth0 masquerade
    sh /snap.sh /vms/resolute-server-cloudimg-amd64_decompressed.img /vms/resolute-server-cloudimg-amd64-vmlinuz-generic /w
  "
