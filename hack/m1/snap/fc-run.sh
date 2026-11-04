#!/bin/sh
# Runs fc.sh in its own container on fleetingd's built image.
# Usage: hack/m1/snap/fc-run.sh [clones] [ci|ubuntu] [copy|reflink]
# reflink: disks live on a loop-mounted btrfs and each clone is a reflink
# copy of the template (copy-on-write, instant). Needs --privileged for loop.
# ci: Firecracker's CI kernel. ubuntu: the image's own kernel (the one
# fleetingd boots in Cloud Hypervisor), uncompressed from its bzImage.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
DATA=${FLEETCI_M1_DATA:-$HOME/.local/share/fleetci-m1}
W=${FLEETCI_FC_DATA:-$HOME/.local/share/fleetci-fc}
CLONES=${1:-3}
FLAVOR=${2:-ci}
DISK=${3:-copy}
priv=
[ "$DISK" = reflink ] && priv=--privileged
FC_VERSION=1.17.0
KERNEL=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155
mkdir -p "$W"

docker run --rm --name fleetci-fc $priv \
  --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --cap-add SYS_ADMIN \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined --sysctl net.ipv4.ip_forward=1 \
  -e CLONES="$CLONES" -e FLAVOR="$FLAVOR" -e DISK="$DISK" -e FC_ARGS="${FC_ARGS:-}" \
  -v "$DATA/vms:/vms:ro" -v "$W:/w" -v "$HERE/fc.sh:/fc.sh:ro" \
  --entrypoint sh fleetci-m1-host -c "
    set -e
    apt-get update -qq && apt-get install -y -qq --no-install-recommends jq bc procps e2fsprogs fdisk util-linux zstd file btrfs-progs >/dev/null
    curl -fsSL https://github.com/firecracker-microvm/firecracker/releases/download/v$FC_VERSION/firecracker-v$FC_VERSION-x86_64.tgz | tar -xz -C /tmp
    install -m 755 /tmp/release-v$FC_VERSION-x86_64/firecracker-v$FC_VERSION-x86_64 /usr/local/bin/firecracker
    [ -f /w/vmlinux ] || curl -fsSL -o /w/vmlinux $KERNEL
    nft add table ip fcnat
    nft add chain ip fcnat post '{ type nat hook postrouting priority 100; }'
    nft add rule ip fcnat post ip saddr 172.16.200.0/30 oifname eth0 masquerade
    work=/w
    if [ \"\$DISK\" = reflink ]; then
      [ -f /w/btrfs.img ] || { truncate -s 60G /w/btrfs.img; mkfs.btrfs -q /w/btrfs.img; }
      # /dev is a copy made at start: loop nodes the kernel adds later are missing.
      for i in 0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do [ -e /dev/loop\$i ] || mknod /dev/loop\$i b 7 \$i; done
      mkdir -p /r && mount -o loop /w/btrfs.img /r
      [ -f /r/root.base.ext4 ] || cp --sparse=always /w/root.base.ext4 /r/
      cp -n /w/key /w/key.pub /w/vmlinux-ubuntu /r/ 2>/dev/null || true
      work=/r
      export CP_FLAGS=--reflink=always
    fi
    sh /fc.sh /vms/resolute-server-cloudimg-amd64_decompressed.img /w/vmlinux \$work
  "
