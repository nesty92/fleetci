#!/bin/sh
# Snapshot clones in fleetingd's network model: every clone gets its own tap
# and /30 in the host's namespace, like a booted fleetingd VM. The snapshot's
# config is edited per clone (tap name, host IP, disk, vsock socket), and a
# guest agent on vsock gives the clone its new address and identity before
# anything else talks to it.
#
# The agent is systemd socket activation: vsock port 52, one root sh per
# connection, stdin from the host. Only the host can reach it, through
# cloud-hypervisor's vsock socket.
set -eu
BASE=$1; KERNEL=$2; W=$3
CLONES=${CLONES:-3}; MEM=${MEM:-6144}; CPUS=${CPUS:-4}
MAC=52:54:00:12:34:56
SUBNET=172.16.200.

now() { date +%s.%N; }
since() { printf '%.3f' "$(echo "$(now) - $1" | bc)"; }
ssh_to() { # ssh_to <ip> <key> <cmd>
  ssh -i "$2" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=1 "ubuntu@$1" "$3"
}
agent() { # agent <vsock socket> < script: run it as root in the guest
  { printf 'CONNECT 52\n'; cat; } | socat -t10 - "UNIX-CONNECT:$1" | sed '1{/^OK /d}'
}

mkdir -p "$W"; cd "$W"
rm -rf snap clone* tpl.* ./*.sock ./*.vsock
[ -f key ] || ssh-keygen -q -t ed25519 -N '' -f key

# --- template ---------------------------------------------------------------
cat > user-data <<EOF
#cloud-config
hostname: template
ssh_authorized_keys: ["$(cat key.pub)"]
write_files:
  - path: /etc/systemd/system/fleetingd-agent.socket
    content: |
      [Socket]
      ListenStream=vsock::52
      Accept=yes
      [Install]
      WantedBy=sockets.target
  - path: /etc/systemd/system/fleetingd-agent@.service
    content: |
      [Service]
      ExecStart=/bin/sh
      StandardInput=socket
      StandardOutput=socket
      StandardError=socket
runcmd:
  - ufw allow from ${SUBNET}0/24 proto tcp to any port 22
  - systemctl daemon-reload
  - systemctl enable --now fleetingd-agent.socket
EOF
echo "instance-id: template" > meta-data
cat > net.yaml <<EOF
version: 2
ethernets:
  veth0:
    match: {macaddress: "$MAC"}
    set-name: veth0
    addresses: [${SUBNET}2/30]
    routes: [{to: default, via: ${SUBNET}1}]
    nameservers: {addresses: [1.1.1.3]}
EOF
cloud-localds -N net.yaml seed.img user-data meta-data
qemu-img create -q -f qcow2 -b "$BASE" -F qcow2 tpl.qcow2 30G

t=$(now)
cloud-hypervisor --api-socket "$W/tpl.sock" --kernel "$KERNEL" \
  --disk path="$W/tpl.qcow2",image_type=qcow2,backing_files=on path="$W/seed.img",readonly=on,image_type=raw \
  --cpus boot=$CPUS --memory size=${MEM}M \
  --net tap=tpl0,mac=$MAC,ip=${SUBNET}1,mask=255.255.255.252 \
  --vsock cid=3,socket="$W/tpl.vsock" \
  --cmdline "console=hvc0 root=/dev/vda1 rw" --console off --serial off >tpl.log 2>&1 &
n=0
until ssh_to ${SUBNET}2 key 'cloud-init status --wait >/dev/null 2>&1; systemctl is-active fleetingd-agent.socket >/dev/null && docker version --format "{{.Server.Version}}"' >/dev/null 2>&1; do
  n=$((n + 1)); [ $n -gt 600 ] && { echo "template never answered"; cat tpl.log; exit 1; }
  sleep 0.2
done
echo "template ready: $(since "$t") s"
ssh_to ${SUBNET}2 key "docker pull -q hello-world >/dev/null" || echo "pull failed"
echo "agent answers: $(echo 'id -un; hostname' | agent "$W/tpl.vsock" | paste -sd' ')"

ch-remote --api-socket "$W/tpl.sock" pause
mkdir -p snap
t=$(now)
ch-remote --api-socket "$W/tpl.sock" snapshot "file://$W/snap"
echo "snapshot: $(since "$t") s"
ch-remote --api-socket "$W/tpl.sock" shutdown-vmm || true
wait 2>/dev/null || true
chmod a-w tpl.qcow2
jq -c '{net: .net, vsock: .vsock, disks: [.disks[].path]}' snap/config.json | cut -c1-300

# --- clones ---------------------------------------------------------------
i=1
while [ $i -le "$CLONES" ]; do
  c=clone$i; base=$((i * 4))
  mkdir -p $c/snap
  ssh-keygen -q -t ed25519 -N '' -f $c/key
  qemu-img create -q -f qcow2 -b "$W/tpl.qcow2" -F qcow2 $c/disk.qcow2
  jq --arg d "$W/$c/disk.qcow2" --arg tap "fdc$i" --arg ip "$SUBNET$((base + 1))" --arg vs "$W/$c.vsock" \
    '(.disks[] | select(.path | endswith("tpl.qcow2")) | .path) = $d
     | .net[0].tap = $tap | .net[0].ip = $ip
     | .vsock.socket = $vs | .console.mode = "Off"' snap/config.json > $c/snap/config.json
  cp snap/state.json $c/snap/
  for f in snap/*; do case $f in snap/config.json|snap/state.json) ;; *) ln -s "$W/$f" "$c/$f" ;; esac; done
  i=$((i + 1))
done

i=1
while [ $i -le "$CLONES" ]; do
  (
    c=clone$i; base=$((i * 4)); gw=$SUBNET$((base + 1)); ip=$SUBNET$((base + 2))
    t=$(now)
    cloud-hypervisor --api-socket "$W/$c.sock" \
      --restore source_url="file://$W/$c/snap",memory_restore_mode=copyonwrite,resume=true >$c/ch.log 2>&1 &
    until [ -S "$W/$c.vsock" ]; do sleep 0.01; done
    restored=$(since "$t")
    # New address, key and identity, all before the first SSH.
    agent "$W/$c.vsock" >$c/agent.log 2>&1 <<EOF
set -e
ip addr flush dev veth0
ip addr add $ip/30 dev veth0
ip route replace default via $gw
mkdir -p /run/systemd/network
sed -i 's|${SUBNET}2/30|$ip/30|; s|via: ${SUBNET}1|via: $gw|' /etc/netplan/50-cloud-init.yaml
echo '$(cat $c/key.pub)' > /home/ubuntu/.ssh/authorized_keys
rm -f /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null; systemctl reload ssh
od -An -tx1 -N16 /dev/urandom | tr -d ' \n' > /etc/machine-id
head -c 512 /dev/urandom > /dev/urandom
chronyc -a makestep >/dev/null 2>&1 || true
hostname clone$i
EOF
    agented=$(since "$t")
    until ssh_to $ip $c/key 'docker version --format "{{.Server.Version}}"' >/dev/null 2>&1; do sleep 0.02; done
    ready=$(since "$t")
    check=$(ssh_to $ip $c/key 'echo $(hostname) $(ip -4 -br a show veth0 | tr -s " " | cut -d" " -f3) $(cut -c1-8 /etc/machine-id); docker run --rm --network none hello-world >/dev/null 2>&1 && echo docker-ok || echo docker-FAILED')
    echo "$c: restored $restored s, agent done $agented s, ssh+docker $ready s | $(echo $check | tr '\n' ' ') | agent: $(tr '\n' ' ' < $c/agent.log | cut -c1-80)"
  ) &
  i=$((i + 1))
done
wait
for pid in $(pgrep -f -- "--restore"); do kill $pid; done
wait 2>/dev/null || true
