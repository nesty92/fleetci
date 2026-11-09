#!/bin/sh
# ssh <machine> <cmd>: as root, from the lab
m=$1; shift
case $m in ctl) ip=10.20.0.10;; eu) ip=10.21.0.10;; us) ip=10.22.0.10;; ap) ip=10.23.0.10;; *) ip=$m;; esac
exec ssh -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=3 -i /data/id_ed25519 root@$ip "$@"
