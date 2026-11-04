#!/bin/sh
# Snapshot experiment: boot one template VM from fleetingd's built image,
# snapshot it once Docker answers, then restore clones and time each one to
# a working `docker version` over SSH.
#
# Runs as root inside the fleetci-snap container (see run.sh). Each clone
# runs in its own network namespace, so all of them keep the template's IP
# and MAC: nothing in the guest has to change for the network to work.
set -eu
BASE=$1          # fleetingd's built qcow2 image (read-only)
KERNEL=$2
W=$3             # work directory on disk
CLONES=${CLONES:-3}
MODE=${MODE:-copyonwrite}
MEM=${MEM:-6144}
CPUS=${CPUS:-4}
MAC=52:54:00:12:34:56
GW=172.16.200.1
IP=172.16.200.2

now() { date +%s.%N; }
since() { echo "$(now) - $1" | bc; }
SSH="ssh -i $W/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=1 ubuntu@$IP"

mkdir -p "$W"
cd "$W"
rm -rf snap clone* tpl.* ./*.sock
[ -f key ] || ssh-keygen -q -t ed25519 -N '' -f key

# --- template -------------------------------------------------------------
cat > user-data <<EOF
#cloud-config
hostname: template
ssh_authorized_keys: ["$(cat key.pub)"]
runcmd:
  - ufw allow from $GW proto tcp to any port 22
  - loginctl enable-linger ubuntu
EOF
echo "instance-id: template" > meta-data
cat > net.yaml <<EOF
version: 2
ethernets:
  veth0:
    match: {macaddress: "$MAC"}
    set-name: veth0
    addresses: [$IP/30]
    routes: [{to: default, via: $GW}]
    nameservers: {addresses: [1.1.1.3]}
EOF
cloud-localds -N net.yaml seed.img user-data meta-data
qemu-img create -q -f qcow2 -b "$BASE" -F qcow2 tpl.qcow2

t=$(now)
cloud-hypervisor --api-socket "$W/tpl.sock" --kernel "$KERNEL" \
  --disk path="$W/tpl.qcow2",image_type=qcow2,backing_files=on path="$W/seed.img",readonly=on,image_type=raw \
  --cpus boot=$CPUS --memory size=${MEM}M \
  --net tap=tap0,mac=$MAC,ip=$GW,mask=255.255.255.252 \
  --cmdline "console=hvc0 root=/dev/vda1 rw" --console file="$W/tpl.console" --serial off \
  >tpl.log 2>&1 &
n=0; until [ $((n += 1)) -gt 600 ] && { echo "template never answered"; cat tpl.log; exit 1; } || $SSH 'cloud-init status --wait >/dev/null 2>&1; docker version --format "{{.Server.Version}}"' >/dev/null 2>&1; do sleep 0.2; done
echo "template cold boot to docker: $(since "$t") s"
$SSH 'systemctl is-enabled fail2ban snapd ModemManager 2>&1 | paste -sd" "; systemd-analyze 2>/dev/null | head -1; systemd-analyze critical-chain 2>/dev/null | head -14; systemd-analyze blame 2>/dev/null | head -10' | sed 's/^/  template: /'

$SSH 'docker pull -q hello-world >/dev/null' || echo "  template: pull failed (no egress?)"

ch-remote --api-socket "$W/tpl.sock" pause
mkdir -p snap
t=$(now)
ch-remote --api-socket "$W/tpl.sock" snapshot "file://$W/snap"
echo "snapshot written: $(since "$t") s, $(du -sh snap | cut -f1) on disk ($(du -sh --apparent-size snap | cut -f1) apparent)"
ch-remote --api-socket "$W/tpl.sock" shutdown-vmm || true
wait 2>/dev/null || true
# tpl.qcow2 is now frozen: every clone's disk is an overlay on top of it.
chmod a-w tpl.qcow2

# --- clones ---------------------------------------------------------------
i=1
while [ $i -le "$CLONES" ]; do
  c=clone$i
  mkdir $c
  qemu-img create -q -f qcow2 -b "$W/tpl.qcow2" -F qcow2 $c/disk.qcow2
  jq --arg d "$W/$c/disk.qcow2" --arg con "$W/$c/console" \
    '(.disks[] | select(.path | endswith("tpl.qcow2")) | .path) = $d | .console.file = $con' \
    snap/config.json > $c/snap.config.json
  mkdir $c/snap
  mv $c/snap.config.json $c/snap/config.json
  cp snap/state.json $c/snap/
  for f in snap/*; do
    case $f in snap/config.json|snap/state.json) ;; *) ln -s "$W/$f" "$c/$f" ;; esac
  done
  ip netns add $c
  ip -n $c link set lo up
  i=$((i + 1))
done

# Restore them all at once, as a burst of jobs would.
i=1
while [ $i -le "$CLONES" ]; do
  c=clone$i
  (
    t=$(now)
    ip netns exec $c cloud-hypervisor --api-socket "$W/$c.sock" \
      --restore source_url="file://$W/$c/snap",memory_restore_mode=$MODE,resume=true >$c/ch.log 2>&1 &
    n=0
    until ip netns exec $c $SSH 'docker version --format "{{.Server.Version}}"' >/dev/null 2>&1; do
      n=$((n + 1)); [ $n -gt 600 ] && { echo "$c: never answered"; tail -3 $c/ch.log; exit 1; }
      sleep 0.05
    done
    ready=$(since "$t")
    t2=$(now)
    # Per-clone identity after restore: clock, host keys, machine id.
    ip netns exec $c $SSH 'sudo chronyc -a makestep >/dev/null 2>&1; sudo rm -f /etc/ssh/ssh_host_*; sudo ssh-keygen -A >/dev/null; od -An -tx1 -N16 /dev/urandom | tr -d " \\n" | sudo tee /etc/machine-id >/dev/null; sudo systemctl reload ssh' >/dev/null 2>&1 || true
    fix=$(since "$t2")
    ok=$(ip netns exec $c $SSH -o StrictHostKeyChecking=no 'docker run --rm --network none hello-world >/dev/null 2>&1 && echo docker-run-ok || echo docker-run-FAILED; cat /etc/machine-id | cut -c1-8')
    echo "$c: restore to docker $ready s, identity fixes $fix s, $(echo $ok | tr '\n' ' ')"
  ) &
  i=$((i + 1))
done
wait

sleep 2
for pid in $(pgrep -f -- "--restore"); do
  awk -v p=$pid '/^Rss/{r=$2} /^Pss:/{s=$2} /^Private_Dirty/{d=$2} END{printf "  pid %s: rss %d MB, pss %d MB, private dirty %d MB\n", p, r/1024, s/1024, d/1024}' /proc/$pid/smaps_rollup
done

for pid in $(pgrep -f -- "--restore"); do kill $pid; done
wait 2>/dev/null || true
i=1; while [ $i -le "$CLONES" ]; do ip netns del clone$i 2>/dev/null || true; i=$((i + 1)); done
