#!/bin/sh
# Cold boot timings, Cloud Hypervisor against Firecracker (Ubuntu's kernel in
# both), on fleetingd's image with 4 vCPU and 6 GB. Every boot is a new
# cloud-init instance, as a job VM is. Runs as root in the container that
# boot-run.sh starts.
set -eu
BASE=$1; W=$2; RUNS=${RUNS:-3}
CPUS=4; MEM=6144
MAC=52:54:00:12:34:56; GW=172.16.200.1; IP=172.16.200.2
BZ=$(dirname "$BASE")/resolute-server-cloudimg-amd64-vmlinuz-generic

now() { date +%s.%N; }
since() { printf '%.2f' "$(echo "$(now) - $1" | bc)"; }
SSH="ssh -i $W/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=1 ubuntu@$IP"
api() { curl -fsS --unix-socket "$1" -X "$2" "http://localhost$3" -H 'Content-Type: application/json' ${4:+-d "$4"}; }

cd "$W"
[ -f key ] || ssh-keygen -q -t ed25519 -N '' -f key
ip tuntap add tap0 mode tap 2>/dev/null || true
ip addr add $GW/30 dev tap0 2>/dev/null || true
ip link set tap0 up

seed() { # seed <instance-id>: user-data, meta-data, network-config
  cat > user-data <<EOF
#cloud-config
ssh_authorized_keys: ["$(cat key.pub)"]
runcmd:
  - ufw allow from $GW proto tcp to any port 22
EOF
  echo "instance-id: $1" > meta-data
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
}

# wait_ready <t0>: prints "ssh <s> ready <s>"
wait_ready() {
  t0=$1; ssh_at=
  n=0
  until $SSH true 2>/dev/null; do n=$((n + 1)); [ $n -gt 1500 ] && { echo "no ssh"; return 1; }; sleep 0.05; done
  ssh_at=$(since "$t0")
  $SSH 'cloud-init status --wait >/dev/null 2>&1; docker version --format "{{.Server.Version}}"' >/dev/null
  echo "ssh $ssh_at ready $(since "$t0")"
}

guest() {
  $SSH 'systemd-analyze 2>/dev/null | head -1 | sed "s/Startup finished in //"; systemd-analyze blame 2>/dev/null | head -4 | tr -s " " | paste -sd","'
}

# --- Cloud Hypervisor: qcow2 overlay, NoCloud seed disk, bzImage ---------
i=1
while [ $i -le "${CH_RUNS:-$RUNS}" ]; do
  seed "ch-$i"
  rm -f ch.qcow2 seed.img
  cloud-localds -N network-config seed.img user-data meta-data
  qemu-img create -q -f qcow2 -b "$BASE" -F qcow2 ch.qcow2
  t=$(now)
  cloud-hypervisor --kernel "$BZ" \
    --disk path="$W/ch.qcow2",image_type=qcow2,backing_files=on path="$W/seed.img",readonly=on,image_type=raw \
    --cpus boot=$CPUS --memory size=${MEM}M --net tap=tap0,mac=$MAC \
    --cmdline "console=hvc0 root=/dev/vda1 rw" --console off --serial off >ch.log 2>&1 &
  pid=$!
  r=$(wait_ready "$t")
  echo "cloud-hypervisor run $i: $r | guest $(guest)"
  kill $pid; wait $pid 2>/dev/null || true
  i=$((i + 1))
done

# --- Firecracker: raw ext4 copy with the seed inside, uncompressed kernel --
[ -f vmlinux-ubuntu ] || {
  off=$(grep -obUaP '\x28\xb5\x2f\xfd' "$BZ" | head -1 | cut -d: -f1)
  tail -c +$((off + 1)) "$BZ" | zstd -dcq > vmlinux-ubuntu 2>/dev/null || true
}
i=1
while [ $i -le "$RUNS" ]; do
  seed "fc-$i"
  t=$(now)
  cp --sparse=always root.base.ext4 fc.ext4
  copy=$(since "$t")
  debugfs -R "cat /etc/fstab" fc.ext4 2>/dev/null | grep -vE '^[^#].*[[:space:]]/boot' > fstab
  debugfs -w fc.ext4 -R "rm /etc/fstab" >/dev/null 2>&1
  debugfs -w fc.ext4 -R "write fstab /etc/fstab" >/dev/null 2>&1
  s=/var/lib/cloud/seed/nocloud
  debugfs -w fc.ext4 -R "mkdir /var/lib/cloud/seed" >/dev/null 2>&1 || true
  debugfs -w fc.ext4 -R "mkdir $s" >/dev/null 2>&1 || true
  for f in user-data meta-data network-config; do debugfs -w fc.ext4 -R "write $f $s/$f" >/dev/null 2>&1; done
  S=$W/fc.sock; rm -f "$S"
  t=$(now)
  firecracker --api-sock "$S" --log-path /dev/null >fc.console 2>&1 &
  pid=$!
  until [ -S "$S" ]; do sleep 0.005; done
  api "$S" PUT /boot-source "{\"kernel_image_path\":\"$W/vmlinux-ubuntu\",\"boot_args\":\"${FC_ARGS:-console=ttyS0 reboot=k panic=1 root=/dev/vda rw}\"}"
  api "$S" PUT /drives/root "{\"drive_id\":\"root\",\"path_on_host\":\"$W/fc.ext4\",\"is_root_device\":true,\"is_read_only\":false}"
  api "$S" PUT /machine-config "{\"vcpu_count\":$CPUS,\"mem_size_mib\":$MEM}"
  api "$S" PUT /network-interfaces/eth0 "{\"iface_id\":\"eth0\",\"guest_mac\":\"$MAC\",\"host_dev_name\":\"tap0\"}"
  api "$S" PUT /actions '{"action_type":"InstanceStart"}'
  r=$(wait_ready "$t")
  echo "firecracker run $i: $r | disk copy $copy | guest $(guest)"
  kill $pid; wait $pid 2>/dev/null || true
  rm -f fc.ext4
  i=$((i + 1))
done
