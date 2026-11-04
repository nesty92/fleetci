#!/bin/bash
# Job VM: Ubuntu 24.04 with Docker, like GitHub's ubuntu-latest. The ubuntu
# user is reached through EC2 Instance Connect with a key that works for 60
# seconds. actions/runner is unpacked here, so GitHub jobs don't download it.
#
# Instance metadata: the VM has no instance profile, so metadata holds no
# credentials. Tokens are required with a hop limit of 1, and containers on
# Docker's networks are blocked from it below. A privileged or host-network
# container could still read it; the VM serves one job and is then removed.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

apt-get update -q
apt-get install -y -q --no-install-recommends docker.io docker-buildx iptables libicu74 git curl ca-certificates
usermod -aG docker ubuntu

mkdir -p /etc/systemd/system/docker.service.d
cat > /etc/systemd/system/docker.service.d/no-imds.conf <<'UNIT'
[Service]
ExecStartPost=/usr/sbin/iptables -I DOCKER-USER -d 169.254.169.254 -j DROP
UNIT

systemctl daemon-reload
systemctl enable --now docker

runner_dir=/home/ubuntu/actions-runner-${runner_version}
install -d -o ubuntu -g ubuntu "$runner_dir"
curl -fsSL "https://github.com/actions/runner/releases/download/v${runner_version}/actions-runner-linux-x64-${runner_version}.tar.gz" |
  sudo -u ubuntu tar -xz -C "$runner_dir"
