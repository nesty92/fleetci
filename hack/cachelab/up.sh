#!/bin/sh
# Runs on the host: starts the cachelab, a container that runs microVMs
# the way fleetingd does, with a Docker Hub pull-through mirror beside it,
# for load/load.py's fleet.
#
#   up.sh
#
# DATA (~/.local/share/fleetci-cachelab) keeps the layers and the mirror's
# images. IMAGES is a directory with fleetingd's prebuilt
# VM image and kernel, CH the cloud-hypervisor binary; both default to the
# local fleet's. The lab image is fleetci-lab (hack/lab).
#
# seccomp-io_uring.json is Docker's default seccomp profile (moby/profiles,
# Apache-2.0) with io_uring_setup, io_uring_enter and io_uring_register
# allowed: the default refuses them, and cloud-hypervisor then gives its
# VMs' disks the synchronous backend, slower than on a host. Stop it all with
#   docker rm -f fleetci-cachelab fleetci-cachelab-mirror
set -eu
here=$(cd "$(dirname "$0")" && pwd)
DATA=${DATA:-$HOME/.local/share/fleetci-cachelab}
IMAGES=${IMAGES:-$HOME/.local/share/fleetci-fleet/w1/var/images}
CH=${CH:-$HOME/.local/share/fleetci-fleet/artifacts/cloud-hypervisor}
mkdir -p "$DATA/mirror"

docker network inspect cachelab >/dev/null 2>&1 ||
  docker network create --subnet 10.231.29.0/24 cachelab >/dev/null
docker inspect fleetci-cachelab-mirror >/dev/null 2>&1 ||
  docker run -d --name fleetci-cachelab-mirror --network cachelab --ip 10.231.29.10 \
    -e REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io \
    -v "$DATA/mirror:/var/lib/registry" registry:3 >/dev/null
docker inspect fleetci-cachelab >/dev/null 2>&1 ||
  docker run -d --init --name fleetci-cachelab --hostname cachelab --network cachelab \
    --memory ${MEM:-20g} --memory-swap ${MEM:-20g} \
    --security-opt seccomp="$here/seccomp-io_uring.json" \
    --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1 \
    -e MIRROR=10.231.29.10:5000 \
    -v "$DATA:/data" -v "$IMAGES:/base:ro" \
    -v "$CH:/usr/local/bin/cloud-hypervisor:ro" -v "$here:/lab:ro" \
    fleetci-lab sleep infinity >/dev/null
docker exec fleetci-cachelab sh /lab/tools.sh
echo "cachelab up"
