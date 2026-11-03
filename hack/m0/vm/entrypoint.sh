#!/bin/sh
# Install this VM's one public key, start sshd, then run Docker on its Unix
# socket only. The private key never leaves fleetci.
set -eu
install -d -m 700 -o fleetci -g fleetci /home/fleetci/.ssh
printf '%s\n' "$VM_AUTHORIZED_KEY" > /home/fleetci/.ssh/authorized_keys
chown fleetci:fleetci /home/fleetci/.ssh/authorized_keys
chmod 600 /home/fleetci/.ssh/authorized_keys
unset VM_AUTHORIZED_KEY
# Host keys are made per VM, at boot, so no two VMs share one.
ssh-keygen -A >/dev/null
/usr/sbin/sshd -e
exec dind dockerd --host=unix:///var/run/docker.sock
