#!/bin/sh
# Start with no authorized keys: the plugin adds a short-lived one on each
# ConnectInfo. Host keys are made here, so no two VMs share one. Docker
# listens on its Unix socket only.
set -eu
install -d -m 700 -o fleetci -g fleetci /home/fleetci/.ssh
install -m 600 -o fleetci -g fleetci /dev/null /home/fleetci/.ssh/authorized_keys
ssh-keygen -A >/dev/null
/usr/sbin/sshd -e
exec dind dockerd --host=unix:///var/run/docker.sock
