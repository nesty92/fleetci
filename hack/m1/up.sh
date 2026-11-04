#!/bin/sh
# M1: M0 with real microVMs. 4vcpu-6gb boots Cloud Hypervisor VMs through
# fleeting-plugin-fleetingd; 1vcpu-2gb stays on dockervm, or with
# FLEETCI_M1_SMALL=fleetingd becomes a second fleetingd group on the same
# host. Everything runs inside the fleetci-m1 container, which gets /dev/kvm
# and its own network namespace for fleetingd's tap devices and nftables
# rules. fleetingd is built from our fork when it is checked out next to
# fleetci (FLEETCI_FLEETINGD_SRC), else the upstream release in the image.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DATA=${FLEETCI_M1_DATA:-$HOME/.local/share/fleetci-m1}
TOKEN_FILE=${FLEETCI_M1_TOKEN:-$HOME/.config/fleetci/m0-runner.token}
NET=fleetci-m0
GITHUB_REPOS=${FLEETCI_GITHUB_REPOS:-}
GITHUB_TOKEN_FILE=${FLEETCI_GITHUB_TOKEN_FILE:-$HOME/.config/fleetci/github.token}
# Optional Actions cache server (hack/cache/up.sh prints its URL).
GITHUB_CACHE_URL=${FLEETCI_GITHUB_CACHE_URL:-}
RUNNER_VERSION=2.338.2
SMALL=${FLEETCI_M1_SMALL:-dockervm}
FD_SRC=${FLEETCI_FLEETINGD_SRC:-$REPO/../fleeting-plugin-fleetingd}

[ -c /dev/kvm ] || { echo "no /dev/kvm" >&2; exit 1; }
umask 077
mkdir -p "$DATA/work" "$DATA/bin" "$DATA/vms" "$DATA/vms-small"

(cd "$REPO" && CGO_ENABLED=0 go build -o "$DATA/bin/" ./cmd/fleeting-plugin-fleetci ./cmd/fleetci ./cmd/fleeting-plugin-dockervm)
fleetingd=/usr/local/bin/fleeting-plugin-fleetingd
if [ -d "$FD_SRC/cmd/fleeting-plugin-fleetingd" ]; then
  # main.go embeds LICENSE and NOTICE, which upstream's CI puts there.
  cp "$FD_SRC/LICENSE" "$FD_SRC/cmd/fleeting-plugin-fleetingd/"
  [ -f "$FD_SRC/cmd/fleeting-plugin-fleetingd/NOTICE" ] || echo "local build" > "$FD_SRC/cmd/fleeting-plugin-fleetingd/NOTICE"
  (cd "$FD_SRC" && CGO_ENABLED=0 go build -o "$DATA/bin/fleeting-plugin-fleetingd" ./cmd/fleeting-plugin-fleetingd)
  fleetingd=$DATA/bin/fleeting-plugin-fleetingd
fi
docker build -q -t fleetci-m0-vm "$REPO/hack/m0/vm" >/dev/null
docker build -q -t fleetci-m1-host "$HERE" >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

github=
if [ -n "$GITHUB_REPOS" ]; then
  [ -r "$GITHUB_TOKEN_FILE" ] || { echo "no GitHub token in $GITHUB_TOKEN_FILE" >&2; exit 1; }
  repos=$(printf '%s\n' $GITHUB_REPOS | jq -R . | jq -sc .)
  github="
[github]
repos = $repos
token_file = \"$GITHUB_TOKEN_FILE\""
  [ -n "$GITHUB_CACHE_URL" ] && github="$github
cache_url = \"$GITHUB_CACHE_URL\""
fi

# fleetingd_class <class> <group name> <subnet> <disk dir> <cpus> <MB> <max>
fleetingd_class() {
  cat <<CLASS
[classes.$1]
plugin = "$fleetingd"
max_instances = $7
connector_config = { username = "ubuntu", timeout = "5m" }

[classes.$1.plugin_config]
name = "$2"
egress_interface = "eth0"
vm_disk_directory = "$4"
vm_subnet = "$3"
vm_num_cpu_cores = $5
vm_memory_mb = $6
vm_disk_size_gb = 30
vm_enable_virtio_console = true
vm_package_upgrade = false
vm_prebuild_cloudinit_extra_cmds = [
  "DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io docker-buildx git zstd",
  "usermod -aG docker ubuntu",
  "su - ubuntu -c 'mkdir -p actions-runner-$RUNNER_VERSION && curl -fsSL https://github.com/actions/runner/releases/download/v$RUNNER_VERSION/actions-runner-linux-x64-$RUNNER_VERSION.tar.gz | tar -xz -C actions-runner-$RUNNER_VERSION'",
  "/home/ubuntu/actions-runner-$RUNNER_VERSION/bin/installdependencies.sh",
  "DEBIAN_FRONTEND=noninteractive apt-get purge -y --auto-remove snapd modemmanager udisks2 multipath-tools open-iscsi unattended-upgrades",
  "systemctl mask fail2ban.service chrony-wait.service",
  "systemctl mask apt-daily.timer apt-daily-upgrade.timer motd-news.timer update-notifier-download.timer fwupd-refresh.timer",
]
CLASS
}

if [ "$SMALL" = fleetingd ]; then
  small=$(fleetingd_class 1vcpu-2gb fd1 172.16.121. "$DATA/vms-small" 1 2048 2)
else
  small='[classes.1vcpu-2gb]
plugin = "'"$DATA"'/bin/fleeting-plugin-dockervm"
max_instances = 3
plugin_config = { name = "1vcpu-2gb", image = "fleetci-m0-vm", network = "'"$NET"'", cpus = "1", memory = "2g" }
connector_config = { username = "fleetci", timeout = "2m" }'
fi

# fleetingd builds its image once (Ubuntu 26.04 cloud image, then these
# commands, then cloud-init clean) and clones it per VM. The commands are
# cloud-init runcmd entries: keep them free of ': ' and ' #'.
cat > "$DATA/fleetci.toml" <<TOML
default_class = "1vcpu-2gb"

$small
$(fleetingd_class 4vcpu-6gb fd4 172.16.120. "$DATA/vms" 4 6144 2)
$github
TOML

"$DATA/bin/fleetci" runner-config -global -config "$DATA/fleetci.toml" \
  -name fleetci-m1 -token-file "$TOKEN_FILE" -plugin "$DATA/bin/fleeting-plugin-fleetci" > "$DATA/config.toml"
chmod 600 "$DATA/config.toml" "$DATA/fleetci.toml"

docker rm -f fleetci-m1 >/dev/null 2>&1 || true
# Same paths inside as outside, so the generated configs work in both.
mounts="-v $DATA:$DATA -v /var/run/docker.sock:/var/run/docker.sock"
[ -n "$GITHUB_REPOS" ] && mounts="$mounts -v $GITHUB_TOKEN_FILE:$GITHUB_TOKEN_FILE:ro"
host="--network $NET --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1 $mounts"
docker run --rm $host --entrypoint "$DATA/bin/fleetci" fleetci-m1-host \
  check -config "$DATA/fleetci.toml" -runner-config "$DATA/config.toml" ||
  echo "fleetci check found problems; see above" >&2
docker run -d --name fleetci-m1 $host fleetci-m1-host \
  run --config "$DATA/config.toml" --working-directory "$DATA/work" >/dev/null

echo "running: docker logs -f fleetci-m1"
echo "  the first 4vcpu-6gb job waits for fleetingd's image build (several minutes)"
echo "  VM consoles: $DATA/vms/.instance_data/*_console"
