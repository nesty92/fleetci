#!/bin/sh
# Runs reip.sh on m1's shared prebuilt image (hack/m1 with our fleetingd fork).
# Usage: hack/m1/snap/reip-run.sh [clones]
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
DATA=${FLEETCI_M1_DATA:-$HOME/.local/share/fleetci-m1}
W=${FLEETCI_REIP_DATA:-$HOME/.local/share/fleetci-reip}
image=$(ls -t "$DATA"/images/prebuilt-*.qcow2 | head -1)
mkdir -p "$W"
docker run --rm --name fleetci-reip \
  --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1 \
  -e CLONES="${1:-3}" -v "$DATA/images:/images:ro" -v "$W:/w" -v "$HERE/reip.sh:/reip.sh:ro" \
  --entrypoint sh fleetci-m1-host -c "
    set -e
    apt-get update -qq && apt-get install -y -qq --no-install-recommends cloud-image-utils jq bc procps socat >/dev/null
    curl -fsSL -o /usr/local/bin/ch-remote https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v54.0/ch-remote-static
    chmod 755 /usr/local/bin/ch-remote
    nft add table ip reipnat
    nft add chain ip reipnat post '{ type nat hook postrouting priority 100; }'
    nft add rule ip reipnat post ip saddr 172.16.200.0/24 oifname eth0 masquerade
    sh /reip.sh /images/$(basename "$image") /images/$(basename "${image%.qcow2}").vmlinuz /w
  "
