#!/bin/sh
# Checks that a built image holds what fleetci needs, and prints its
# manifest. Usage, as root with the nbd module: verify.sh IMAGE.qcow2 RUNNER_VERSION
set -eu
image=$1 runner=$2
dev=/dev/nbd0
mnt=$(mktemp -d)
modprobe nbd max_part=8
qemu-nbd --read-only --connect=$dev "$image"
cleanup() { umount "$mnt" 2>/dev/null || true; qemu-nbd --disconnect $dev >/dev/null || true; rmdir "$mnt"; }
trap cleanup EXIT
for _ in 1 2 3 4 5 6 7 8 9 10; do [ -b ${dev}p1 ] && break; sleep 1; done
mount -o ro,noload ${dev}p1 "$mnt"

fail=0
need() { if [ -e "$mnt/$1" ]; then echo "ok    $1"; else echo "MISSING $1"; fail=1; fi; }
need usr/bin/docker
need usr/libexec/docker/cli-plugins/docker-buildx
need usr/libexec/docker/cli-plugins/docker-compose
need usr/bin/git
need usr/bin/git-lfs
need usr/bin/gh
need usr/bin/zstd
need usr/bin/unzip
need usr/bin/make
need usr/bin/gcc
need usr/bin/python
need usr/bin/pip3
need var/lib/systemd/linger/ubuntu
need usr/bin/cloud-init
need home/ubuntu/actions-runner-$runner/run.sh
need etc/fleetci-image
if ls "$mnt"/usr/lib/*/libicuuc.so.* >/dev/null 2>&1; then echo "ok    libicu"; else echo "MISSING libicu"; fail=1; fi
if grep -q '^docker:.*ubuntu' "$mnt/etc/group"; then echo "ok    ubuntu in the docker group"; else echo "MISSING ubuntu in the docker group"; fail=1; fi
echo "--- manifest"
cat "$mnt/etc/fleetci-image" 2>/dev/null || true
grep -h '^serial' "$mnt/etc/cloud/build.info" 2>/dev/null | sed 's/^serial: */ubuntu=/' || true
exit $fail
