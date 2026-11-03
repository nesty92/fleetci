#!/bin/bash
# Job VM: Docker only. ec2-user is reached through EC2 Instance Connect with
# a key that works for 60 seconds.
#
# Instance metadata: the VM has no instance profile, so metadata holds no
# credentials. Tokens are required with a hop limit of 1, and containers on
# Docker's networks are blocked from it below. A privileged or host-network
# container could still read it; the VM serves one job and is then removed.
set -euo pipefail

dnf install -y iptables-nft docker
usermod -aG docker ec2-user

mkdir -p /etc/systemd/system/docker.service.d
cat > /etc/systemd/system/docker.service.d/no-imds.conf <<'UNIT'
[Service]
ExecStartPost=/usr/sbin/iptables -I DOCKER-USER -d 169.254.169.254 -j DROP
UNIT

systemctl daemon-reload
systemctl enable --now docker
