#!/bin/sh
# Runs in the cachelab container, after replay.sh has run once (it makes
# the network and key): can a cache disk be added to a VM that is already
# running, as fleetingd's are when fleetci gets one? And to one restored
# from a snapshot, as fleetingd's VMs are made? Times each step.
#
# The VM runs under landlock, as fleetingd runs it. It may use what the
# rules name, so the cache directory has to be among them from the start;
# a disk outside them is tried first and must be refused.
#
# /data/chapi is a static build of a small client for the API socket.
set -eu
D=/data V=/data/s5 C=/data/hotplug
ip=10.29.0.15 mac=52:54:00:29:00:15 tap=tap-c5
BASE=$(ls /base/prebuilt-*.qcow2 | head -1)
KERNEL=${BASE%.qcow2}.vmlinuz
api=$V/api.sock dev=/dev/disk/by-id/virtio-cache
now() { date +%s.%N; }
el() { awk -v a="$(now)" -v b="$1" 'BEGIN{printf "%.2f", a-b}'; }
ssh_() { ssh -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=2 -i $D/id_ed25519 root@$ip "$@"; }
chapi() { $D/chapi $api "$@"; }
waitssh() {
  end=$(($(date +%s) + 120))
  until ssh_ true 2>/dev/null; do [ "$(date +%s)" -lt "$end" ] || { echo "no ssh"; exit 1; }; sleep 0.2; done
}
# Adds <layer> as the cache disk and waits for the guest to see it.
attach() {
  t=$(now)
  # As fleetingd's disk attach asks for it. The serial names it in the
  # guest, whatever else it has.
  chapi vm.add-disk "{\"path\": \"$1\", \"id\": \"cache\", \"serial\": \"cache\", \"image_type\": \"Qcow2\", \"backing_files\": true}"; echo
  end=$(($(date +%s) + 10))
  until ssh_ test -b $dev; do
    [ "$(date +%s)" -lt "$end" ] || { echo "  no $dev after 10s"; ssh_ 'ls -l /dev/disk/by-id/; dmesg | tail -n 5'; exit 1; }
    sleep 0.05
  done
  echo "  add-disk until $dev: $(el $t)s"
}
# Unmounts it in the guest, removes it, and checks the layer.
detach() {
  t=$(now)
  ssh_ 'sync && umount /cache'
  chapi vm.remove-device '{"id": "cache"}'
  until ! ssh_ test -b $dev; do sleep 0.05; done
  echo "  umount and remove-device: $(el $t)s"
  qemu-img check -q "$1" && echo "  $(basename "$1") is clean"
}

mkdir -p $V $C
ip link show $tap >/dev/null 2>&1 || { ip tuntap add $tap mode tap; ip link set $tap master br-c up; }
if [ ! -f $V/seed.img ]; then
  printf 'version: 2\nethernets:\n  lan:\n    match: {macaddress: "%s"}\n    set-name: lan\n    addresses: [%s/24]\n    routes: [{to: default, via: 10.29.0.1}]\n    nameservers: {addresses: [1.1.1.1, 8.8.8.8]}\n' $mac $ip > $V/net.yaml
  printf '#cloud-config\ndisable_root: false\nusers:\n  - name: root\n    ssh_authorized_keys: ["%s"]\nbootcmd:\n  - [ufw, disable]\n' "$(cat $D/id_ed25519.pub)" > $V/user-data
  printf 'instance-id: hotplug\nlocal-hostname: hotplug\n' > $V/meta-data
  cloud-localds -N $V/net.yaml $V/seed.img $V/user-data $V/meta-data
fi
rm -rf $V/root.qcow2 $V/snap $api $C/*.qcow2 $D/outside.qcow2
qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" $V/root.qcow2 40G

echo "1. a running VM"
cloud-hypervisor --kernel "$KERNEL" --cmdline "console=ttyS0 root=/dev/vda1 rw" \
  --disk path=$V/root.qcow2,image_type=qcow2,backing_files=on path=$V/seed.img,readonly=on,image_type=raw \
  --cpus boot=2 --memory size=2048M --net tap=$tap,mac=$mac --serial file=$V/serial.log --console off \
  --api-socket path=$api \
  --landlock --landlock-rules path=$V,access=rw path=$C,access=rw path=/base,access=r path=/sys,access=r \
  > $V/ch.log 2>&1 &
ch=$!
trap 'kill $ch 2>/dev/null || true' EXIT
waitssh

qemu-img create -q -f qcow2 $D/outside.qcow2 1G
if chapi vm.add-disk "{\"path\": \"$D/outside.qcow2\", \"id\": \"outside\", \"image_type\": \"Qcow2\"}" > $V/outside.err 2>&1; then
  echo "  a disk outside the landlock rules was added: unexpected"
else
  echo "  a disk outside the landlock rules is refused: $(cat $V/outside.err | tr -d "\n")"
fi
rm -f $D/outside.qcow2

qemu-img create -q -f qcow2 $C/cache-1.qcow2 30G
attach $C/cache-1.qcow2
t=$(now)
ssh_ 'mkfs.ext4 -q -E lazy_itable_init=1,lazy_journal_init=1 '$dev' && mkdir -p /cache && mount -o noatime '$dev' /cache &&
  dd if=/dev/urandom of=/cache/blob bs=1M count=512 status=none && sha256sum /cache/blob > /cache/blob.sum'
echo "  mkfs, mount, write 512 MB: $(el $t)s"
detach $C/cache-1.qcow2

echo "2. a VM restored from a snapshot of it"
mkdir -p $V/snap && chapi vm.pause
chapi vm.snapshot "{\"destination_url\": \"file://$V/snap\"}"
kill $ch; wait $ch 2>/dev/null || true
rm -f $api
t=$(now)
cloud-hypervisor --api-socket path=$api \
  --restore source_url=file://$V/snap,memory_restore_mode=copyonwrite,resume=true \
  > $V/ch.log 2>&1 &
ch=$!
waitssh
echo "  restored and answering ssh: $(el $t)s"
# The next job's layer, on the first.
qemu-img create -q -f qcow2 -F qcow2 -b $C/cache-1.qcow2 $C/cache-2.qcow2 30G
attach $C/cache-2.qcow2
t=$(now)
ssh_ 'mount -o noatime '$dev' /cache && cd /cache && sha256sum -c --quiet blob.sum && echo "  the first layer'"'"'s data reads back"'
echo "  mount and read 512 MB: $(el $t)s"
detach $C/cache-2.qcow2
echo "  layer 2 is $(du -m $C/cache-2.qcow2 | cut -f1) MB"
grep -i -E 'error|landlock' $V/ch.log | tail -n 3 || true
