#!/bin/sh
# Runs in the cachelab: installs what fleetingd and the lab's checks use
# that the fleetci-lab image leaves out. up.sh runs it on a new container.
set -eu
need=""
for c in nft:nftables qemu-img:qemu-utils mkfs.ext4:e2fsprogs debugfs:e2fsprogs \
  iptables:iptables ip:iproute2 ssh:openssh-client curl:curl zstd:zstd fstrim:util-linux; do
  command -v "${c%%:*}" >/dev/null || need="$need ${c#*:}"
done
[ -z "$need" ] && exit 0
echo "installing:$need"
apt-get update -qq >/dev/null
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends $need >/dev/null
