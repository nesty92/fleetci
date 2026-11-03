#!/bin/sh
# Set the login password, start sshd, then run Docker on its Unix socket only.
set -eu
echo "fleetci:${VM_PASSWORD}" | chpasswd >/dev/null
unset VM_PASSWORD
/usr/sbin/sshd -e
exec dind dockerd --host=unix:///var/run/docker.sock
