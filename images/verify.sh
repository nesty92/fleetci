#!/bin/sh
# Checks that a built image holds what fleetci needs, and prints its
# manifest. Usage, as root with the nbd module:
#   verify.sh IMAGE.qcow2 github RUNNER_VERSION
#   verify.sh IMAGE.qcow2 gitlab
set -eu
image=$1 kind=$2 runner=${3:-}
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
need usr/bin/zstd
need usr/bin/cloud-init
need var/lib/systemd/linger/ubuntu
need etc/fleetci-image
if grep -q '^docker:.*ubuntu' "$mnt/etc/group"; then echo "ok    ubuntu in the docker group"; else echo "MISSING ubuntu in the docker group"; fail=1; fi
gone() { if [ -e "$mnt/$1" ]; then echo "LEFT  $1"; fail=1; else echo "ok    no $1"; fi; }
gone usr/bin/gitlab-runner
gone usr/bin/snap
case $kind in
github)
  need usr/libexec/docker/cli-plugins/docker-compose
  need usr/bin/git
  need usr/bin/git-lfs
  need usr/bin/gh
  need usr/bin/unzip
  need usr/bin/zip
  need usr/bin/make
  need usr/bin/gcc
  need usr/bin/python
  need usr/bin/pip3
  need home/ubuntu/actions-runner-$runner/run.sh
  need home/ubuntu/actions-runner-$runner/bin/Runner.Listener
  if ls "$mnt"/usr/lib/*/libicuuc.so.* >/dev/null 2>&1; then echo "ok    libicu"; else echo "MISSING libicu"; fail=1; fi
  ;;
gitlab) ;;
*) echo "unknown image $kind" >&2; fail=1 ;;
esac
echo "--- manifest"
cat "$mnt/etc/fleetci-image" 2>/dev/null || true
grep -h '^serial' "$mnt/etc/cloud/build.info" 2>/dev/null | sed 's/^serial: */ubuntu=/' || true
exit $fail
