#!/bin/sh
# Runs fleetci on this machine: gitlab-runner with fleetci's plugin, and
# optionally the GitHub side, in the fleetci-local container. Two classes,
# 1vcpu-2gb and 4vcpu-8gb, each a fleeting plugin's group:
#
#   FLEETCI_LOCAL_VMS=microvm    both on fleeting-plugin-fleetingd (Cloud
#                                Hypervisor microVMs); needs /dev/kvm
#   FLEETCI_LOCAL_VMS=container  both on fleeting-plugin-dockervm (privileged
#                                containers on this machine's Docker)
#   FLEETCI_LOCAL_VMS=mixed      1vcpu-2gb on dockervm, 4vcpu-8gb on fleetingd
#
# The default is microvm when /dev/kvm exists, else container. fleetingd's
# tap devices and nftables rules live in the container's network namespace,
# so the machine's network and firewall are left alone. fleetingd is our
# fork: built from its checkout next to fleetci (FLEETCI_FLEETINGD_SRC) when
# there is one, else its release in the image.
#
# The runner token is read from ~/.config/fleetci/runner.token and only
# written into the 0600 config.toml.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DATA=${FLEETCI_LOCAL_DATA:-$HOME/.local/share/fleetci-local}
TOKEN_FILE=${FLEETCI_LOCAL_TOKEN:-$HOME/.config/fleetci/runner.token}
NET=fleetci-local
GITHUB_REPOS=${FLEETCI_GITHUB_REPOS:-}
GITHUB_TOKEN_FILE=${FLEETCI_GITHUB_TOKEN_FILE:-$HOME/.config/fleetci/github.token}
# Optional Actions cache server (hack/cache/up.sh prints its URL).
GITHUB_CACHE_URL=${FLEETCI_GITHUB_CACHE_URL:-}
RUNNER_VERSION=2.338.2
if [ -c /dev/kvm ]; then VMS=${FLEETCI_LOCAL_VMS:-microvm}; else VMS=${FLEETCI_LOCAL_VMS:-container}; fi
FD_SRC=${FLEETCI_FLEETINGD_SRC:-$REPO/../fleeting-plugin-fleetingd}
# Restore microVMs from a snapshot instead of booting them.
SNAPSHOT=${FLEETCI_LOCAL_SNAPSHOT:-true}
# serve: GitHub jobs run in a "fleetci serve" of their own, on groups of
# their own, instead of inside gitlab-runner's fleetci plugin.
GITHUB_MODE=${FLEETCI_LOCAL_GITHUB:-plugin}
# true: 4vcpu-8gb's jobs beyond its 2 VMs go to 4vcpu-8gb-cloud, containers
# standing in for a cloud's VMs (fleetci's overflow).
OVERFLOW=${FLEETCI_LOCAL_OVERFLOW:-false}
# n: each VM runs n jobs before it is removed (max_use_count), GitLab only;
# fleetci refuses it together with GitHub.
REUSE=${FLEETCI_LOCAL_REUSE:-}
# [host] for fleetci.toml, such as 'cpus = 5, memory = "10GB"': a budget
# smaller than this machine, so jobs wait for room. Unset, it is detected.
HOST=${FLEETCI_LOCAL_HOST:-}

case $VMS in
  microvm) small=microvm big=microvm ;;
  container) small=container big=container ;;
  mixed) small=container big=microvm ;;
  *) echo "FLEETCI_LOCAL_VMS: microvm, container or mixed, not $VMS" >&2; exit 1 ;;
esac
[ "$big" = container ] || [ -c /dev/kvm ] || { echo "no /dev/kvm; try FLEETCI_LOCAL_VMS=container" >&2; exit 1; }
[ -r "$TOKEN_FILE" ] || { echo "no runner token in $TOKEN_FILE" >&2; exit 1; }

umask 077
mkdir -p "$DATA/work" "$DATA/bin" "$DATA/images"

sh "$REPO/hack/build-l1.sh"
(cd "$REPO" && CGO_ENABLED=0 go build -o "$DATA/bin/" ./cmd/fleeting-plugin-fleetci ./cmd/fleetci ./cmd/fleeting-plugin-dockervm)
fleetingd=/usr/local/bin/fleeting-plugin-fleetingd
if [ "$big" = microvm ] && [ -d "$FD_SRC/cmd/fleeting-plugin-fleetingd" ]; then
  # main.go embeds LICENSE and NOTICE, which upstream's CI puts there.
  cp "$FD_SRC/LICENSE" "$FD_SRC/cmd/fleeting-plugin-fleetingd/"
  [ -f "$FD_SRC/cmd/fleeting-plugin-fleetingd/NOTICE" ] || echo "local build" > "$FD_SRC/cmd/fleeting-plugin-fleetingd/NOTICE"
  (cd "$FD_SRC" && CGO_ENABLED=0 go build -o "$DATA/bin/fleeting-plugin-fleetingd" ./cmd/fleeting-plugin-fleetingd)
  fleetingd=$DATA/bin/fleeting-plugin-fleetingd
fi
{ [ "$small" = container ] || [ "$OVERFLOW" = true ]; } && docker build -q -t fleetci-local-vm "$HERE/vm" >/dev/null
docker build -q -t fleetci-local-host "$HERE" >/dev/null
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

# class <container|microvm> <class> <group> <cpus> <MB> <max> <subnet> [overflow]
# A group is one plugin's set of VMs; each needs its own name, and a microVM
# group its own subnet and disk directory.
class() {
  if [ "$1" = container ]; then
    cat <<CLASS
[classes.$2]
plugin = "$DATA/bin/fleeting-plugin-dockervm"
max_instances = $6${8:+
overflow = \"$8\"}${REUSE:+
max_use_count = $REUSE}
plugin_config = { name = "$3", image = "fleetci-local-vm", network = "$NET", cpus = "$4", memory = "$5m" }
connector_config = { username = "fleetci", timeout = "2m" }
CLASS
    return
  fi
  mkdir -p "$DATA/vms-$3"
  # fleetingd builds its image once (Ubuntu 26.04 cloud image, then these
  # commands, then cloud-init clean) and clones it per VM. The commands are
  # cloud-init runcmd entries: keep them free of ': ' and ' #'.
  cat <<CLASS
[classes.$2]
plugin = "$fleetingd"
max_instances = $6${8:+
overflow = \"$8\"}${REUSE:+
max_use_count = $REUSE}
connector_config = { username = "ubuntu", timeout = "5m" }

[classes.$2.plugin_config]
name = "$3"
egress_interface = "eth0"
vm_disk_directory = "$DATA/vms-$3"
vm_image_directory = "$DATA/images"
vm_subnet = "$7"
vm_num_cpu_cores = $4
vm_memory_mb = $5
vm_disk_size_gb = 30
vm_enable_virtio_console = true
vm_package_upgrade = false
vm_prebuild_on_start = true
vm_snapshot = $SNAPSHOT
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

plugin_github=$github
rm -f "$DATA/fleetci-github.toml"
if [ "$GITHUB_MODE" = serve ] && [ -n "$github" ]; then
  plugin_github=
  cat > "$DATA/fleetci-github.toml" <<TOML
default_class = "1vcpu-2gb"

$(class "$small" 1vcpu-2gb gh1 1 2048 1 172.16.123.)
$(class "$big" 4vcpu-8gb gh4 4 8192 1 172.16.122.)
$github
TOML
fi
cat > "$DATA/fleetci.toml" <<TOML
default_class = "1vcpu-2gb"${HOST:+
host = { $HOST \}}

$(class "$small" 1vcpu-2gb vm1 1 2048 3 172.16.121.)
$(if [ "$OVERFLOW" = true ]; then
  class "$big" 4vcpu-8gb vm4 4 8192 2 172.16.120. 4vcpu-8gb-cloud
  echo
  class container 4vcpu-8gb-cloud cloud4 4 8192 3
else
  class "$big" 4vcpu-8gb vm4 4 8192 2 172.16.120.
fi)
$plugin_github
TOML

"$DATA/bin/fleetci" runner-config -global -config "$DATA/fleetci.toml" \
  -name fleetci-local -token-file "$TOKEN_FILE" -plugin "$DATA/bin/fleeting-plugin-fleetci" > "$DATA/config.toml"

docker rm -f fleetci-local >/dev/null 2>&1 || true
# Same paths inside as outside, so the generated configs work in both.
host="--network $NET -v $DATA:$DATA -v /var/run/docker.sock:/var/run/docker.sock"
[ -n "$GITHUB_REPOS" ] && host="$host -v $GITHUB_TOKEN_FILE:$GITHUB_TOKEN_FILE:ro"
[ "$big" = microvm ] && host="$host --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1"
for config in fleetci.toml fleetci-github.toml; do
  [ -f "$DATA/$config" ] || continue
  runner=$DATA/config.toml
  [ "$config" = fleetci.toml ] || runner=
  docker run --rm $host --entrypoint "$DATA/bin/fleetci" fleetci-local-host \
    check -config "$DATA/$config" -runner-config "$runner" ||
    echo "fleetci check found problems in $config; see above" >&2
done
# tini reaps what gitlab-runner leaves; gitlab-runner runs in a loop with
# fleetci's watchdog beside it, which stops it when its fleetci plugin is
# gone (gitlab-runner never starts a dead plugin again).
docker run -d --init --name fleetci-local $host --entrypoint sh fleetci-local-host -c '
  "$0/bin/fleetci" watchdog -plugin "$0/bin/fleeting-plugin-fleetci" \
    -restart "p=\$(pgrep -x gitlab-runner); kill \$p; sleep 15; kill -9 \$p 2>/dev/null || true" &
  if [ -f "$0/fleetci-github.toml" ]; then
    while :; do "$0/bin/fleetci" serve -config "$0/fleetci-github.toml"; sleep 2; done &
  fi
  while :; do gitlab-runner run --config "$0/config.toml" --working-directory "$0/work"; sleep 1; done
' "$DATA" >/dev/null

echo "running ($VMS): docker logs -f fleetci-local"
[ "$big" = microvm ] && echo "  the first microVM waits for fleetingd's image build (several minutes, once)"
exit 0
