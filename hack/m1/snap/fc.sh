#!/bin/sh
# The snapshot experiment of snap.sh on Firecracker: same image (fleetingd's
# Ubuntu 26.04 with Docker and actions/runner), same sizes, same clones.
#
# Firecracker's CI kernel has no GPT, ISO9660/FAT or nf_tables, so the root
# partition is extracted to a plain ext4 file, the cloud-init seed is written
# into it (NoCloud's seed directory), and the guest switches to
# iptables-legacy before Docker's first start.
#
# Each clone runs in its own network and mount namespace: same tap, IP and
# MAC as the template, and its own disk bind-mounted over the template's
# path, which the snapshot names.
set -eu
BASE=$1          # fleetingd's built qcow2 image (read-only)
KERNEL=$2        # Firecracker CI vmlinux
W=$3
CLONES=${CLONES:-3}
MEM=${MEM:-6144}
CPUS=${CPUS:-4}
MAC=52:54:00:12:34:56
GW=172.16.200.1
IP=172.16.200.2

# FLAVOR=ubuntu boots the image's own kernel, the one fleetingd gives Cloud
# Hypervisor, uncompressed from its bzImage (Firecracker takes ELF only).
LEGACY_IPTABLES=1
if [ "${FLAVOR:-ci}" = ubuntu ]; then
  bz=$(dirname "$BASE")/resolute-server-cloudimg-amd64-vmlinuz-generic
  off=$(grep -obUaP '\x28\xb5\x2f\xfd' "$bz" | head -1 | cut -d: -f1)
  tail -c +$((off + 1)) "$bz" | zstd -dcq > "$W/vmlinux-ubuntu" 2>/dev/null || true
  echo "kernel: $(file -b "$W/vmlinux-ubuntu" | cut -c1-80)"
  KERNEL=$W/vmlinux-ubuntu
  LEGACY_IPTABLES=0
fi
now() { date +%s.%N; }
since() { echo "$(now) - $1" | bc; }
SSH="ssh -i $W/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=1 ubuntu@$IP"
api() { # api <socket> <method> <path> [json]
  curl -fsS --unix-socket "$1" -X "$2" "http://localhost$3" -H 'Content-Type: application/json' ${4:+-d "$4"}
}

mkdir -p "$W"
cd "$W"
rm -rf snap clone* tpl.* ./*.sock
[ -f key ] || ssh-keygen -q -t ed25519 -N '' -f key

# --- root filesystem ----------------------------------------------------
if [ ! -f root.base.ext4 ]; then
  t=$(now)
  qemu-img convert -O raw "$BASE" disk.raw
  start=$(sfdisk -J disk.raw | jq '.partitiontable.partitions[] | select(.node | endswith("1")) | .start')
  size=$(sfdisk -J disk.raw | jq '.partitiontable.partitions[] | select(.node | endswith("1")) | .size')
  dd if=disk.raw of=root.base.ext4 bs=512 skip="$start" count="$size" conv=sparse status=none
  rm disk.raw
  echo "root partition extracted: $(since "$t") s, $(du -h root.base.ext4 | cut -f1) used"
fi

cat > user-data <<EOF
#cloud-config
hostname: template
ssh_authorized_keys: ["$(cat key.pub)"]
runcmd:
  - ufw allow from $GW proto tcp to any port 22
  - loginctl enable-linger ubuntu
EOF
# Firecracker's CI kernel has no nf_tables: Docker needs iptables-legacy.
if [ "${LEGACY_IPTABLES:-1}" = 1 ]; then
  cat >> user-data <<EOF
bootcmd:
  - update-alternatives --set iptables /usr/sbin/iptables-legacy
  - update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy
  - systemctl restart docker
EOF
fi
echo "instance-id: template" > meta-data
cat > network-config <<EOF
version: 2
ethernets:
  veth0:
    match: {macaddress: "$MAC"}
    set-name: veth0
    addresses: [$IP/30]
    routes: [{to: default, via: $GW}]
    nameservers: {addresses: [1.1.1.3]}
EOF

t=$(now)
cp ${CP_FLAGS:---sparse=always} root.base.ext4 tpl.ext4
echo "template disk copied: $(since "$t") s"
# Only the root partition is here: drop the EFI (and any other) mounts, or
# boot waits 90 s for the device and stops in emergency mode.
debugfs -R "cat /etc/fstab" tpl.ext4 2>/dev/null | grep -vE '^[^#].*[[:space:]]/boot' > fstab
debugfs -w tpl.ext4 -R "rm /etc/fstab" >/dev/null 2>&1
debugfs -w tpl.ext4 -R "write fstab /etc/fstab" >/dev/null 2>&1
sed 's/^/  fstab: /' fstab
seed=/var/lib/cloud/seed/nocloud
debugfs -w tpl.ext4 -R "mkdir /var/lib/cloud/seed" >/dev/null 2>&1 || true
debugfs -w tpl.ext4 -R "mkdir $seed" >/dev/null 2>&1 || true
for f in user-data meta-data network-config; do
  debugfs -w tpl.ext4 -R "write $f $seed/$f" >/dev/null 2>&1
done

# --- template ------------------------------------------------------------
ip tuntap add tap0 mode tap
ip addr add $GW/30 dev tap0
ip link set tap0 up

S=$W/tpl.sock
firecracker --api-sock "$S" --log-path /dev/null >tpl.console 2>&1 &
until [ -S "$S" ]; do sleep 0.01; done
api "$S" PUT /boot-source "{\"kernel_image_path\":\"$KERNEL\",\"boot_args\":\"${FC_ARGS:-console=ttyS0 reboot=k panic=1 root=/dev/vda rw}\"}"
api "$S" PUT /drives/root "{\"drive_id\":\"root\",\"path_on_host\":\"$W/tpl.ext4\",\"is_root_device\":true,\"is_read_only\":false}"
api "$S" PUT /machine-config "{\"vcpu_count\":$CPUS,\"mem_size_mib\":$MEM}"
api "$S" PUT /network-interfaces/eth0 "{\"iface_id\":\"eth0\",\"guest_mac\":\"$MAC\",\"host_dev_name\":\"tap0\"}"
api "$S" PUT /entropy '{}' || true
t=$(now)
api "$S" PUT /actions '{"action_type":"InstanceStart"}'
n=0
until $SSH 'cloud-init status --wait >/dev/null 2>&1; docker version --format "{{.Server.Version}}"' >/dev/null 2>&1; do
  n=$((n + 1)); [ $n -gt 600 ] && { echo "template never answered"; tail -30 tpl.console; exit 1; }
  sleep 0.2
done
echo "template cold boot to docker: $(since "$t") s"
$SSH 'uname -r; systemd-analyze 2>/dev/null | head -1; systemd-analyze blame 2>/dev/null | head -6' | sed 's/^/  template: /'
$SSH 'docker pull -q hello-world >/dev/null && docker run --rm hello-world >/dev/null && echo "  template: docker run ok"' || echo "  template: docker run FAILED"

api "$S" PATCH /vm '{"state":"Paused"}'
mkdir -p snap
t=$(now)
api "$S" PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$W/snap/vmstate\",\"mem_file_path\":\"$W/snap/mem\"}"
echo "snapshot written: $(since "$t") s, $(du -sh snap | cut -f1) on disk"
kill %1 2>/dev/null || pkill -f "api-sock $S" || true
wait 2>/dev/null || true
ip link del tap0
chmod a-w tpl.ext4

# --- clones -------------------------------------------------------------
i=1
while [ $i -le "$CLONES" ]; do
  c=clone$i
  mkdir $c
  t=$(now)
  cp ${CP_FLAGS:---sparse=always} tpl.ext4 $c/disk.ext4
  chmod u+w $c/disk.ext4
  echo "$c disk copied: $(since "$t") s"
  ip netns add $c
  ip -n $c link set lo up
  ip -n $c tuntap add tap0 mode tap
  ip -n $c addr add $GW/30 dev tap0
  ip -n $c link set tap0 up
  i=$((i + 1))
done

i=1
while [ $i -le "$CLONES" ]; do
  c=clone$i
  (
    S=$W/$c.sock
    # Own mount namespace: this clone's disk sits at the path the snapshot names.
    ip netns exec $c unshare -m sh -c "mount --bind $W/$c/disk.ext4 $W/tpl.ext4 && exec firecracker --api-sock $S --log-path /dev/null" >$c/console 2>&1 &
    until [ -S "$S" ]; do sleep 0.005; done
    t=$(now)
    api "$S" PUT /snapshot/load "{\"snapshot_path\":\"$W/snap/vmstate\",\"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$W/snap/mem\"},\"resume_vm\":true}"
    n=0
    until ip netns exec $c $SSH 'docker version --format "{{.Server.Version}}"' >/dev/null 2>&1; do
      n=$((n + 1)); [ $n -gt 600 ] && { echo "$c: never answered"; tail -5 $c/console; exit 1; }
      sleep 0.05
    done
    ready=$(since "$t")
    t2=$(now)
    # VMGenID already reseeded the guest's RNG; clock, host keys, machine id.
    ip netns exec $c $SSH 'sudo chronyc -a makestep >/dev/null 2>&1; sudo rm -f /etc/ssh/ssh_host_*; sudo ssh-keygen -A >/dev/null; od -An -tx1 -N16 /dev/urandom | tr -d " \n" | sudo tee /etc/machine-id >/dev/null; sudo systemctl reload ssh' >/dev/null 2>&1 || true
    fix=$(since "$t2")
    ok=$(ip netns exec $c $SSH 'docker run --rm --network none hello-world >/dev/null 2>&1 && echo docker-run-ok || echo docker-run-FAILED; cut -c1-8 /etc/machine-id')
    echo "$c: restore to docker $ready s, identity fixes $fix s, $(echo $ok | tr '\n' ' ')"
  ) &
  i=$((i + 1))
done
wait

sleep 2
for pid in $(pgrep -x firecracker); do
  awk -v p=$pid '/^Rss/{r=$2} /^Pss:/{s=$2} /^Private_Dirty/{d=$2} END{printf "  pid %s: rss %d MB, pss %d MB, private dirty %d MB\n", p, r/1024, s/1024, d/1024}' /proc/$pid/smaps_rollup
done
pkill -x firecracker || true
i=1; while [ $i -le "$CLONES" ]; do ip netns del clone$i 2>/dev/null || true; i=$((i + 1)); done
