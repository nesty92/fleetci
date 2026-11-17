#!/bin/bash
# The measurement host's first boot: Ubuntu 24.04 on an instance with
# nested virtualization. Installs fleetci, Cloud Hypervisor, fleetingd and
# fleeting-plugin-aws, and writes /etc/fleetci/fleetci.toml with both arms'
# classes. fleetci-serve starts once the operator puts the GitHub token in
# /etc/fleetci/github.token (README, step 3); no secret is in this file.
# Progress: /var/log/cloud-init-output.log; done when /etc/fleetci/ready exists.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

# Without /dev/kvm the instance has no nested virtualization: stop here,
# so a run can't measure microVMs that aren't there.
if [ ! -c /dev/kvm ]; then
  mkdir -p /etc/fleetci
  echo "no /dev/kvm: is nested virtualization enabled on this instance?" | tee /etc/fleetci/NO-KVM
  exit 1
fi

apt-get update -q
apt-get install -y -q --no-install-recommends ca-certificates curl jq nftables qemu-utils sysstat
echo net.ipv4.ip_forward=1 > /etc/sysctl.d/90-fleetci.conf
sysctl --system >/dev/null

get() { # <file> <url>: into /usr/local/bin
  curl -fsSL --retry 5 --retry-all-errors -o "/usr/local/bin/$1" "$2"
  chmod 755 "/usr/local/bin/$1"
}
get cloud-hypervisor \
  https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v54.0/cloud-hypervisor-static
curl -fsSL --retry 5 --retry-all-errors \
  https://github.com/nesty92/fleeting-plugin-fleetingd/releases/download/v0.4.0-fleetci.4/fleeting-plugin-fleetingd_linux_amd64.tar.gz |
  tar -xz -C /usr/local/bin fleeting-plugin-fleetingd
get fleeting-plugin-aws \
  https://gitlab.com/api/v4/projects/39457165/packages/generic/releases/v1.3.1/fleeting-plugin-aws-linux-amd64

d=$(mktemp -d)
base=https://github.com/nesty92/fleetci/releases/download/v${fleetci_version}
curl -fsSL --retry 5 --retry-all-errors -o "$d/fleetci-linux-amd64" "$base/fleetci-linux-amd64"
curl -fsSL --retry 5 --retry-all-errors -o "$d/checksums.txt" "$base/checksums.txt"
(cd "$d" && sha256sum --ignore-missing -c checksums.txt)
install -m 755 "$d/fleetci-linux-amd64" /usr/local/bin/fleetci
rm -rf "$d"

# The microVMs reach the internet through the host. Tokens with one hop
# already keep the host's role from them; this drops their packets to the
# metadata service outright. fleetingd's own tables are left alone.
install -d -m 0755 /etc/fleetci
cat > /etc/fleetci/imds-guard.nft <<'NFT'
table inet fleetci_imds_guard
delete table inet fleetci_imds_guard
table inet fleetci_imds_guard {
  chain forward {
    type filter hook forward priority -10; policy accept;
    ip saddr 172.16.0.0/12 ip daddr 169.254.169.254 drop
  }
}
NFT
cat > /etc/systemd/system/fleetci-imds-guard.service <<'UNIT'
[Unit]
Description=Keep the microVMs from the host's instance metadata
Before=fleetci-serve.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/fleetci/imds-guard.nft

[Install]
WantedBy=multi-user.target
UNIT

egress=$(ip -o route get 1.1.1.1 | sed -n 's/.* dev \([^ ]*\).*/\1/p')
install -d -m 0700 /var/lib/fleetci
install -d -m 0755 /var/lib/fleetci/images-4 /var/lib/fleetci/images-2 \
  /var/lib/fleetci/images-4n /var/lib/fleetci/images-2n /var/lib/fleetci/cache-4 /var/lib/fleetci/cache-2

# Both arms in one fleetci, so they differ only by the class a job asks for:
#   host-*        microVMs on this host, each job's own cache disk
#   host-*-nodisk the same microVMs without one
#   ec2-*         one EC2 VM per job, from the baseline Auto Scaling groups
# All microVM classes start from the same published image, pinned, built
# from images/github.json: the baseline AMI is baked from the same recipe.
cat > /etc/fleetci/fleetci.toml <<'TOML'
default_class = "host-2vcpu-4gb"
state_dir = "/var/lib/fleetci"
usage_file = "/var/lib/fleetci/usage.db"
metrics_listen = "127.0.0.1:9482"

[github]
repos = ["${github_repo}"]
token_file = "/etc/fleetci/github.token"

# The load's main branch writes the caches its pull requests start from,
# protected or not.
[github.cache_disk]
writers = ["default"]
TOML

microvm() { # <class> <name> <subnet> <cpus> <memory MB> <images> [cache dir]
  cat >> /etc/fleetci/fleetci.toml <<TOML

[classes.$1]
plugin = "fleeting-plugin-fleetingd"
max_instances = 8
connector_config = { username = "ubuntu", timeout = "5m" }

[classes.$1.plugin_config]
name = "$2"
egress_interface = "$egress"
vm_subnet = "$3"
vm_disk_directory = "/var/lib/fleetci/vms-$2"
vm_image_directory = "$6"
vm_num_cpu_cores = $4
vm_memory_mb = $5
vm_disk_size_gb = 30
vm_enable_virtio_console = true
vm_package_upgrade = false
vm_snapshot = true
vm_image_url = "https://github.com/nesty92/fleetci/releases/download/vm-image-github-20261116-23/github-{arch}.SHA256SUMS"
TOML
  if [ -n "$${7:-}" ]; then
    cat >> /etc/fleetci/fleetci.toml <<TOML
vm_cache_directory = "$7"

[classes.$1.cache_disk]
budget = "60GB"
min_free = "20GB"
max_per_cache = "20GB"
TOML
  fi
}
microvm host-4vcpu-8gb        m4  172.16.120. 4 8192 /var/lib/fleetci/images-4  /var/lib/fleetci/cache-4
microvm host-2vcpu-4gb        m2  172.16.121. 2 4096 /var/lib/fleetci/images-2  /var/lib/fleetci/cache-2
microvm host-4vcpu-8gb-nodisk m4n 172.16.122. 4 8192 /var/lib/fleetci/images-4n
microvm host-2vcpu-4gb-nodisk m2n 172.16.123. 2 4096 /var/lib/fleetci/images-2n

%{ for class, c in vm_classes ~}
cat >> /etc/fleetci/fleetci.toml <<'TOML'

[classes.${class}]
plugin = "fleeting-plugin-aws"
max_instances = ${c.max_instances}
plugin_config = { name = "fleetci-measure-${class}", region = "${region}" }
connector_config = { username = "ubuntu", timeout = "5m" }
TOML
%{ endfor ~}

cat > /etc/systemd/system/fleetci-serve.service <<'UNIT'
[Unit]
Description=fleetci: GitHub Actions jobs on microVMs and EC2 VMs
After=network-online.target fleetci-imds-guard.service
Wants=network-online.target
Requires=fleetci-imds-guard.service
ConditionPathExists=/etc/fleetci/github.token

[Service]
ExecStart=/usr/local/bin/fleetci serve -config /etc/fleetci/fleetci.toml
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT

# The host's CPU every 10 s, steal included: how busy the microVMs kept
# it, and what the hypervisor under it took.
install -d -m 0755 /var/log/fleetci-measure
cat > /etc/systemd/system/fleetci-measure-vmstat.service <<'UNIT'
[Unit]
Description=Sample the host's CPU and memory every 10 s

[Service]
ExecStart=/bin/sh -c 'exec /usr/bin/vmstat -n -t -w 10 >> /var/log/fleetci-measure/vmstat.log'
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now fleetci-imds-guard fleetci-measure-vmstat
systemctl enable fleetci-serve
date -u +%FT%TZ > /etc/fleetci/ready
