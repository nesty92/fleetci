#!/bin/sh
# Runs in the lab: the machines as they were before fleetci
# init, without rebuilding them; the workers keep the job image they were
# given, and lose fleetci, its service and the snapshot their first check
# made, as a machine that never joined. The controller keeps fleetci.
set -eu
S=/data/ssh.sh
stop='kill $(pidof fleetci) 2>/dev/null || true; end=$(($(date +%s)+30)); while pidof fleetci >/dev/null; do [ $(date +%s) -lt $end ] || { kill -9 $(pidof fleetci); }; sleep 1; done; kill $(pidof cloud-hypervisor) 2>/dev/null || true'
unit='systemctl disable --now fleetci-worker 2>/dev/null; rm -f /etc/systemd/system/fleetci-worker.service; systemctl daemon-reload'
for m in eu us ap; do $S $m "$unit" & done; wait
for m in ctl eu us ap; do $S $m "$stop" & done; wait
$S ctl 'rm -rf /etc/fleetci/pki /var/lib/fleetci /root/token* /root/.bash_history'
for m in eu us ap; do
  $S $m 'rm -rf /etc/fleetci /var/lib/fleetci/vms-* /var/lib/fleetci/images/snapshot-* /root/token /root/.bash_history
         rm -f /usr/local/bin/fleetci /usr/local/bin/cloud-hypervisor /usr/local/bin/fleeting-plugin-fleetingd' &
done
wait
echo rewound
