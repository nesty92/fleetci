#!/bin/bash
# qcow2 overlays against raw reflink clones for VM disks, on a real XFS
# partition. Run as root on a throwaway machine with /dev/kvm, such as a
# GitHub-hosted runner: it REFORMATS whatever disk is mounted at /mnt, or
# else makes a 60 GB /xfs.img. Needs qemu-utils, xfsprogs,
# cloud-image-utils and cloud-hypervisor. Usage: run.sh OUTDIR [ROUNDS]
#
# Each trial boots N VMs at once (1 vCPU, 2 GB each), every one on its own
# new disk made from the same prepared image, with the host's page cache
# either warm (the base image just read, as after earlier jobs) or cold.
set -euo pipefail
out=$(realpath "$1") rounds=${2:-3}
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$out"
exec > >(tee -a "$out/run.log") 2>&1
ms() { echo $(( $(date +%s%N) / 1000000 )); }

echo "== host"
nproc; free -g | head -2; uname -r
lsblk -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,MODEL
swapon --show || true

# A spare disk if the runner has one (whatever /mnt is mounted from, if it
# isn't /). GitHub's runners now have one disk, so otherwise XFS goes on a
# loop device with direct I/O over a preallocated file on the ext4 root:
# no second page cache under XFS, and blocks already allocated beneath.
src=$(findmnt -n -o SOURCE /mnt || true)
root=$(findmnt -n -o SOURCE /)
if [ -n "$src" ] && [ "$src" != "$root" ]; then
  echo "== formatting spare disk $src as XFS"
  echo "xfs_on=partition $src" > "$out/xfs-on"
  swapoff -a || true
  umount /mnt
else
  fallocate -l 60G /xfs.img
  src=$(losetup --direct-io=on --show -f /xfs.img)
  echo "== no spare disk: XFS on $src, direct I/O over a preallocated file"
  echo "xfs_on=loop-dio $src" > "$out/xfs-on"
  losetup -l "$src"
fi
mkfs.xfs -f -q "$src"
mkdir -p /bench && mount -o noatime "$src" /bench
xfs_info /bench | grep -o 'reflink=[01]'
df -h /bench
B=/bench

echo "== image"
rel=resolute
curl -fsSL -o $B/cloud.img https://cloud-images.ubuntu.com/$rel/current/$rel-server-cloudimg-amd64.img
curl -fsSL -o $B/vmlinuz https://cloud-images.ubuntu.com/$rel/current/unpacked/$rel-server-cloudimg-amd64-vmlinuz-generic
qemu-img convert -O qcow2 $B/cloud.img $B/base.qcow2 && rm $B/cloud.img
qemu-img resize -q $B/base.qcow2 12G
mkdir -p $B/files && cp "$here/guest.sh" $B/files/
mkfs.ext4 -q -d $B/files $B/files.img 16M
cloud-localds -N "$here/network-config" $B/seed.img "$here/user-data"

ip tuntap add tap0 mode tap && ip addr add 192.168.249.1/24 dev tap0 && ip link set tap0 up
sysctl -q -w net.ipv4.ip_forward=1
iptables -P FORWARD ACCEPT
iptables -t nat -A POSTROUTING -s 192.168.249.0/24 -j MASQUERADE
echo "== preparing the image (fio, data set, bench service)"
t=$(ms)
timeout 900 cloud-hypervisor --kernel $B/vmlinuz --cmdline "console=ttyS0 root=/dev/vda1 rw" \
  --disk path=$B/base.qcow2,image_type=qcow2 path=$B/seed.img,image_type=raw,readonly=on path=$B/files.img,image_type=raw,readonly=on \
  --cpus boot=4 --memory size=4G --net tap=tap0,mac=52:54:00:12:34:56 \
  --serial file=$out/prep-serial.log --console off > $out/prep-ch.log 2>&1
grep -q PREP-DONE $out/prep-serial.log || { tail -50 $out/prep-serial.log; echo "::error::prep failed"; exit 1; }
grep -a '^tree:' $out/prep-serial.log || true
echo "prepared in $(( ($(ms) - t) / 1000 )) s"
t=$(ms); qemu-img convert -O raw $B/base.qcow2 $B/base.raw; echo "base.raw made in $(( $(ms) - t )) ms"
qemu-img info $B/base.qcow2 | grep -E 'virtual|disk size'
# Today's fleetci: qcow2 overlays on the host's ext4, here its real partition.
E=/var/bench-ext4 && mkdir -p $E && cp $B/base.qcow2 $E/base.qcow2
du -h --apparent-size $B/base.raw; du -h $B/base.raw

results=$out/results.tsv
echo -e "round\tdisk\tcache\tvms\tvm\tkey\tvalue" > $results
trial() { # round disk cache nvms
  local r=$1 disk=$2 cache=$3 n=$4 i pids=() t0
  for i in $(seq $n); do
    t0=$(ms)
    if [ $disk = qcow2 ]; then
      qemu-img create -q -f qcow2 -F qcow2 -b $B/base.qcow2 $B/vm$i.disk 12G
    elif [ $disk = ext4qcow2 ]; then
      qemu-img create -q -f qcow2 -F qcow2 -b $E/base.qcow2 $E/vm$i.disk 12G && ln -sf $E/vm$i.disk $B/vm$i.disk
    else
      cp --reflink=always $B/base.raw $B/vm$i.disk
    fi
    echo -e "$r\t$disk\t$cache\t$n\t$i\tmake_disk_ms\t$(( $(ms) - t0 ))" >> $results
  done
  sync
  if [ $cache = cold ]; then
    echo 3 > /proc/sys/vm/drop_caches
  else
    echo 3 > /proc/sys/vm/drop_caches
    case $disk in
    qcow2) cat $B/base.qcow2 ;; ext4qcow2) cat $E/base.qcow2 ;; *) cat $B/base.raw ;;
    esac > /dev/null
  fi
  local io0; io0=$(awk '/some/ {sub("total=","",$5); print $5}' /proc/pressure/io)
  t0=$(ms)
  for i in $(seq $n); do
    local d="path=$B/vm$i.disk,image_type=raw"
    [ $disk != reflink ] && d="path=$(readlink -f $B/vm$i.disk),image_type=qcow2,backing_files=on"
    (
      s0=$(ms)
      timeout 600 cloud-hypervisor --kernel $B/vmlinuz --cmdline "console=ttyS0 root=/dev/vda1 rw quiet bench.vm=$i" \
        --disk "$d" --cpus boot=1 --memory size=2G \
        --serial file=$out/s-$r-$disk-$cache-$n-$i.log --console off > $out/ch-$r-$disk-$cache-$n-$i.log 2>&1 || echo "vm $i exited $?"
      echo -e "$r\t$disk\t$cache\t$n\t$i\twall_ms\t$(( $(ms) - s0 ))" >> $results
    ) &
    pids+=($!)
  done
  for i in $(seq $n); do
    wait ${pids[$((i-1))]} || true
  done
  local io1; io1=$(awk '/some/ {sub("total=","",$5); print $5}' /proc/pressure/io)
  echo -e "$r\t$disk\t$cache\t$n\t0\thost_io_wait_ms\t$(( (io1 - io0) / 1000 ))" >> $results
  for i in $(seq $n); do
    # The first line can share the console with the login prompt.
    grep -ao 'BENCH [a-z_0-9= ]*' $out/s-$r-$disk-$cache-$n-$i.log | sed 's/^BENCH //' | tr ' ' '\n' | { grep = || true; } |
      while IFS== read -r k v; do echo -e "$r\t$disk\t$cache\t$n\t$i\t$k\t$v"; done >> $results
    grep -aq 'BENCH done=1' $out/s-$r-$disk-$cache-$n-$i.log || echo "::warning::vm $i of $r/$disk/$cache/$n didn't finish"
  done
  echo "$(date +%T) round $r $disk $cache $n VMs: $(( ($(ms) - t0) / 1000 )) s"
  rm -f $B/vm*.disk $E/vm*.disk
}

echo "== trials"
grep -ah 'io_uring\|backend' $out/prep-ch.log | head -3 || true
for r in $(seq $rounds); do
  # Every combination once per round, in a random order.
  for c in $(printf '%s\n' {ext4qcow2,qcow2,reflink}-{warm,cold}-{1,4} | shuf); do
    IFS=- read -r disk cache n <<<"$c"
    trial $r $disk $cache $n
  done
done
grep -ah 'io_uring' $out/ch-*.log | sort | uniq -c | head -5 || true
{ cat "$out/xfs-on"; python3 "$here/summary.py" $results; } | tee $out/summary.md
