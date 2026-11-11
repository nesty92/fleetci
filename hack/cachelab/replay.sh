#!/bin/sh
# Runs in the cachelab container: one CI job of a real commit in a fresh
# microVM, as fleetingd starts one, with a second disk for caches.
#
#   replay.sh <n> <parent layer | none> <repo> <commit> [cache: yes|no]
#
# The VM's cache disk is a qcow2 overlay, /data/cache-<n>.qcow2, on
# <parent>; after the job it is kept as the next layer. The root disk is
# thrown away. The source is /srcs/<repo>-<commit>.tar (prep.sh), and the
# steps are the repo's CI: fleetci's go vet, go test -race and GoReleaser
# snapshot, or fleetingd's cross-build.
#
# SLOT (0-9) runs several at once, each with its own address and files;
# CPUS and MEM (MB) size the VM, 4 and 6144 by default.
set -eu
n=$1 parent=$2 repo=$3 commit=$4 usecache=${5:-yes}
slot=${SLOT:-0} cpus=${CPUS:-4} mem=${MEM:-6144}
D=/data
V=$D/s$slot
ip=10.29.0.1$slot mac=52:54:00:29:00:1$slot tap=tap-c$slot
BASE=$(ls /base/prebuilt-*.qcow2 | head -1)
KERNEL=${BASE%.qcow2}.vmlinuz
now() { date +%s.%N; }
el() { awk -v a="$(now)" -v b="$1" 'BEGIN{printf "%.2f", a-b}'; }
ssh_() { ssh -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=2 -i $D/id_ed25519 root@$ip "$@"; }

# The network, once, and a tap per slot.
mkdir -p $V
ip link show br-c >/dev/null 2>&1 || {
  ip link add br-c type bridge; ip addr add 10.29.0.1/24 dev br-c; ip link set br-c up
  OUT=$(ip route show default | awk '{print $5; exit}')
  iptables -t nat -A POSTROUTING -s 10.29.0.0/24 -o "$OUT" -j MASQUERADE
}
ip link show $tap >/dev/null 2>&1 || { ip tuntap add $tap mode tap; ip link set $tap master br-c up; }
[ -f $D/id_ed25519 ] || ssh-keygen -q -t ed25519 -N '' -f $D/id_ed25519
if [ ! -f $V/seed.img ]; then
  cat > $V/net.yaml <<NET
version: 2
ethernets:
  lan:
    match: {macaddress: "$mac"}
    set-name: lan
    addresses: [$ip/24]
    routes: [{to: default, via: 10.29.0.1}]
    nameservers: {addresses: [1.1.1.1, 8.8.8.8]}
NET
  # The image's firewall lets in only fleetingd's host; ssh comes from here.
  cat > $V/user-data <<UD
#cloud-config
disable_root: false
users:
  - name: root
    ssh_authorized_keys: ["$(cat $D/id_ed25519.pub)"]
bootcmd:
  - [ufw, disable]
UD
  printf 'instance-id: cachelab-%s\nlocal-hostname: job%s\n' $slot $slot > $V/meta-data
  cloud-localds -N $V/net.yaml $V/seed.img $V/user-data $V/meta-data
fi

t=$(now)
rm -f $V/root.qcow2
qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" $V/root.qcow2 40G
disks="path=$V/root.qcow2,image_type=qcow2,backing_files=on path=$V/seed.img,readonly=on,image_type=raw"
layer=$D/cache-$n.qcow2
if [ "$usecache" = yes ]; then
  if [ "$parent" = none ]; then
    qemu-img create -q -f qcow2 "$layer" 30G
  else
    qemu-img create -q -f qcow2 -F qcow2 -b "$parent" "$layer" 30G
  fi
  disks="$disks path=$layer,image_type=qcow2,backing_files=on"
fi

cloud-hypervisor --kernel "$KERNEL" --cmdline "console=ttyS0 root=/dev/vda1 rw" \
  --disk $disks --cpus boot=$cpus --memory size=${mem}M \
  --net tap=$tap,mac=$mac --serial file=$V/serial.log --console off \
  > $V/ch.log 2>&1 &
ch=$!
trap 'kill $ch 2>/dev/null || true' EXIT
end=$(($(date +%s) + 180))
until ssh_ true 2>/dev/null; do
  [ "$(date +%s)" -lt "$end" ] || { echo "no ssh"; tail -n 20 $V/serial.log; exit 1; }
  sleep 0.2
done
boot_t=$(el $t)

# Mount the cache and put docker's and containerd's data on it, what
# fleetci would do before the job. Both: docker keeps its image layers in
# containerd's directory.
t=$(now)
if [ "$usecache" = yes ]; then
  ssh_ 'set -e
    blkid /dev/vdc >/dev/null || mkfs.ext4 -q -E lazy_itable_init=1,lazy_journal_init=1 /dev/vdc
    mkdir -p /cache && mount -o noatime /dev/vdc /cache
    mkdir -p /cache/docker /cache/containerd /cache/go
    systemctl stop docker.socket docker.service containerd.service
    mount --bind /cache/docker /var/lib/docker
    mount --bind /cache/containerd /var/lib/containerd
    systemctl start containerd docker'
fi
mount_t=$(el $t)

# The commit's source, then its repo's CI. Each step's time; a failed
# step is noted, not fatal.
ssh_ 'rm -rf /src && mkdir -p /src /cache/go && tar -x -C /src && cd /src && git init -q && git add -A &&
  git -c user.name=ci -c user.email=ci@example.invalid commit -qm ci' < /srcs/$repo-$commit.tar
GO="docker run --rm --init -v /src:/src -v /cache/go:/cache/go -e GOCACHE=/cache/go/build -e GOMODCACHE=/cache/go/mod -e GOFLAGS=-buildvcs=false -w /src"
step() { # <label> <command in the VM>
  t=$(now); st=ok
  ssh_ "$2" > $V/step.log 2>&1 || { st=FAILED; tail -n 5 $V/step.log | sed "s/^/  $1: /" >&2; }
  steps="$steps, $1 $(el $t)s${st#ok}"
}
steps=""
imgs=golang:1.27; [ "$repo" != fleetci ] || imgs="$imgs goreleaser/goreleaser:v2.14.0"
step pull "for i in $imgs; do docker pull -q \$i >/dev/null || exit 1; done"
if [ "$repo" = fleetci ]; then
  step vet "$GO golang:1.27 go vet ./..."
  step test "$GO golang:1.27 go test -race ./..."
  # GoReleaser's image has Go 1.26; fleetci's go.mod asks for 1.27.
  step release "$GO -e GOTOOLCHAIN=auto --entrypoint sh goreleaser/goreleaser:v2.14.0 -c 'git config --global --add safe.directory /src && goreleaser release --snapshot --clean'"
else
  # The CI's license step downloads a tool; LICENSE and the template stand in.
  step build "$GO golang:1.27 sh -c 'cp LICENSE NOTICE.tpl cmd/fleeting-plugin-fleetingd/ && mv cmd/fleeting-plugin-fleetingd/NOTICE.tpl cmd/fleeting-plugin-fleetingd/NOTICE && go get . && for a in amd64 arm64 riscv64; do CGO_ENABLED=0 GOOS=linux GOARCH=\$a go build -o dist/x ./cmd/fleeting-plugin-fleetingd/ || exit 1; done'"
fi

# The job ends: the guest flushes and powers off, and its cache overlay
# is the next layer.
t=$(now)
ssh_ 'sync; systemctl poweroff' || true
wait $ch || true
down_t=$(el $t)

size=$( [ "$usecache" = yes ] && du -m "$layer" | cut -f1 || echo -)
echo "$repo $commit job $n (cache $usecache): boot ${boot_t}s, mount ${mount_t}s$steps, shutdown ${down_t}s, layer ${size} MB"
