#!/bin/sh
# Runs in the lab container, as root: the regions' networks and the fleet's
# machines, as cloud-hypervisor microVMs booted from fleetingd's prebuilt
# Ubuntu image (/base, read-only) with a disk overlay each.
#
#   ctl  10.20.0.10  "core", the controller
#   eu   10.21.0.10  +20 ms to the controller
#   us   10.22.0.10  +80 ms
#   ap   10.23.0.10  +160 ms
set -eu
D=/data
BASE=$(ls /base/prebuilt-*.qcow2 | head -1)
KERNEL=${BASE%.qcow2}.vmlinuz
OUT=$(ip route show default | awk '{print $5; exit}')

install -m 755 /lab/ssh.sh $D/ssh.sh
[ -f $D/id_ed25519 ] || ssh-keygen -q -t ed25519 -N '' -f $D/id_ed25519
iptables -t nat -C POSTROUTING -s 10.20.0.0/14 -o "$OUT" -j MASQUERADE 2>/dev/null ||
  iptables -t nat -A POSTROUTING -s 10.20.0.0/14 -o "$OUT" -j MASQUERADE

# machine <name> <net index> <delay ms> <cpus> <memory MB>
machine() {
  n=$1 net=$2 delay=$3 cpus=$4 mem=$5
  br=br-$n tap=tap-$n m=$D/$n
  mkdir -p "$m"
  ip link show $br >/dev/null 2>&1 || {
    ip link add $br type bridge
    ip addr add 10.2$net.0.1/24 dev $br
    ip link set $br up
  }
  # The region's distance: a delay on the way in, so a round trip to the
  # controller takes that much longer.
  [ "$delay" = 0 ] || tc qdisc replace dev $br root netem delay ${delay}ms
  ip link show $tap >/dev/null 2>&1 || {
    ip tuntap add $tap mode tap
    ip link set $tap master $br up
  }
  mac=52:54:00:20:0$net:10
  [ -f "$m/disk.qcow2" ] || qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" "$m/disk.qcow2" 40G
  cat > "$m/net.yaml" <<NET
version: 2
ethernets:
  lan:
    match: {macaddress: "$mac"}
    set-name: lan
    addresses: [10.2$net.0.10/24]
    routes: [{to: default, via: 10.2$net.0.1}]
    nameservers: {addresses: [1.1.1.1, 8.8.8.8]}
NET
  cat > "$m/user-data" <<UD
#cloud-config
hostname: $n
disable_root: false
users:
  - name: root
    ssh_authorized_keys: ["$(cat $D/id_ed25519.pub)"]
write_files:
  - path: /etc/hosts
    append: true
    content: |
      10.20.0.10 ctl
      10.21.0.10 eu
      10.22.0.10 us
      10.23.0.10 ap
  - path: /root/.bashrc
    append: true
    content: |
      PS1='\[\e[1;36m\]\h\[\e[0m\]:\w# '
# The image is fleetingd's job image, whose firewall lets in only the
# host: these machines take ssh and fleetci's own ports.
bootcmd:
  - [ufw, disable]
packages: [nftables, qemu-utils]
runcmd:
  # The image's docker would drop the forwarded traffic of the microVMs
  # a worker runs: these machines don't need it.
  - [systemctl, disable, --now, docker.socket, docker.service, containerd.service]
  - [sh, -c, "iptables -P FORWARD ACCEPT; iptables -F; iptables -t nat -F"]
  - [touch, /run/lab-ready]
UD
  printf 'instance-id: lab-%s-1\nlocal-hostname: %s\n' "$n" "$n" > "$m/meta-data"
  cloud-localds -N "$m/net.yaml" "$m/seed.img" "$m/user-data" "$m/meta-data"
  # free_page_reporting: the guest hands the host back the memory its
  # job VMs free, so a worker holds what it uses, not all of ${mem}M.
  nohup cloud-hypervisor --kernel "$KERNEL" --cmdline "console=ttyS0 root=/dev/vda1 rw" \
    --disk path="$m/disk.qcow2",image_type=qcow2,backing_files=on path="$m/seed.img",readonly=on,image_type=raw \
    --cpus boot=$cpus,nested=on --memory size=${mem}M \
    --balloon size=0,free_page_reporting=on \
    --net tap=$tap,mac=$mac --serial file="$m/serial.log" --console off \
    > "$m/ch.log" 2>&1 &
  echo $! > "$m/pid"
}

machine ctl 0 0 2 1536
machine eu 1 20 3 4608
machine us 2 80 3 4608
machine ap 3 160 3 4608
echo booting
