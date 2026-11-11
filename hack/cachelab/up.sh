#!/bin/sh
# Runs on the host: starts the cachelab, a container that runs microVMs
# the way fleetingd does, with a Docker Hub pull-through mirror beside it,
# and writes the matrix's sources for it.
#
#   up.sh <download dir> [name...]
#
# DATA (~/.local/share/fleetci-cachelab) keeps the layers, the mirror's
# images and the sources. IMAGES is a directory with fleetingd's prebuilt
# VM image and kernel, CH the cloud-hypervisor binary; both default to the
# local fleet's. The lab image is fleetci-lab (hack/lab). Stop it all with
#   docker rm -f fleetci-cachelab fleetci-cachelab-mirror
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dl=$1; shift
DATA=${DATA:-$HOME/.local/share/fleetci-cachelab}
IMAGES=${IMAGES:-$HOME/.local/share/fleetci-fleet/w1/var/images}
CH=${CH:-$HOME/.local/share/fleetci-fleet/artifacts/cloud-hypervisor}
mkdir -p "$DATA/mirror" "$DATA/srcs"
sh "$here/prep-matrix.sh" "$dl" "$DATA/srcs" "$@"

docker network inspect cachelab >/dev/null 2>&1 ||
  docker network create --subnet 10.231.29.0/24 cachelab >/dev/null
docker inspect fleetci-cachelab-mirror >/dev/null 2>&1 ||
  docker run -d --name fleetci-cachelab-mirror --network cachelab --ip 10.231.29.10 \
    -e REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io \
    -v "$DATA/mirror:/var/lib/registry" registry:3 >/dev/null
docker inspect fleetci-cachelab >/dev/null 2>&1 ||
  docker run -d --init --name fleetci-cachelab --hostname cachelab --network cachelab \
    --memory 14g --memory-swap 14g \
    --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1 \
    -e MIRROR=10.231.29.10:5000 \
    -v "$DATA:/data" -v "$DATA/srcs:/srcs:ro" -v "$IMAGES:/base:ro" \
    -v "$CH:/usr/local/bin/cloud-hypervisor:ro" -v "$here:/lab:ro" \
    fleetci-lab sleep infinity >/dev/null
echo "cachelab up: docker exec fleetci-cachelab sh /lab/matrix.sh [name...]"
