#!/bin/sh
# Prepares the local proof: the VM image, its network, the plugin binaries and
# gitlab-runner's config.toml. The runner token is read from
# ~/.config/fleetci/m0-runner.token and only written into the 0600 config.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DATA=${FLEETCI_M0_DATA:-$HOME/.local/share/fleetci-m0}
TOKEN_FILE=${FLEETCI_M0_TOKEN:-$HOME/.config/fleetci/m0-runner.token}
NET=fleetci-m0

umask 077
mkdir -p "$DATA/work" "$DATA/bin"

(cd "$REPO" && go build -o "$DATA/bin/" ./cmd/fleeting-plugin-fleetci ./cmd/fleeting-plugin-dockervm)
docker build -q -t fleetci-m0-vm "$HERE/vm" >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

# gitlab-runner 19.5 dials with a zero timeout when connector_config has
# none, which fails every SSH attempt at once. Both connectors set it.
#
# The VM password is a placeholder: the runner's VM connection ends inside
# the plugin, which reaches the VM with that VM's own key.
cat > "$DATA/config.toml" <<TOML
concurrent = 4
check_interval = 3
log_level = "info"

[[runners]]
  name = "fleetci-m0"
  url = "https://gitlab.com"
  token = "$(cat "$TOKEN_FILE")"
  executor = "docker-autoscaler"
  [runners.docker]
    image = "alpine:3.22"
  [runners.autoscaler]
    plugin = "$DATA/bin/fleeting-plugin-fleetci"
    capacity_per_instance = 4
    max_use_count = 0
    max_instances = 1
    [runners.autoscaler.plugin_config]
      default_class = "1vcpu-2gb"
      # 1vcpu-2gb keeps one VM booted and waiting; 4vcpu-6gb boots per job.
      [runners.autoscaler.plugin_config.classes.1vcpu-2gb]
        plugin = "$DATA/bin/fleeting-plugin-dockervm"
        max_instances = 4
        idle_count = 1
        idle_time = "20m"
        [runners.autoscaler.plugin_config.classes.1vcpu-2gb.plugin_config]
          name = "1vcpu-2gb"
          image = "fleetci-m0-vm"
          network = "$NET"
          cpus = "1"
          memory = "2g"
        [runners.autoscaler.plugin_config.classes.1vcpu-2gb.connector_config]
          username = "fleetci"
          timeout = "2m"
      [runners.autoscaler.plugin_config.classes.4vcpu-6gb]
        plugin = "$DATA/bin/fleeting-plugin-dockervm"
        max_instances = 2
        idle_count = 0
        [runners.autoscaler.plugin_config.classes.4vcpu-6gb.plugin_config]
          name = "4vcpu-6gb"
          image = "fleetci-m0-vm"
          network = "$NET"
          cpus = "4"
          memory = "6g"
        [runners.autoscaler.plugin_config.classes.4vcpu-6gb.connector_config]
          username = "fleetci"
          timeout = "2m"
    [runners.autoscaler.connector_config]
      timeout = "2m"
    [[runners.autoscaler.policy]]
      idle_count = 1
      idle_time = "30m"
    [runners.autoscaler.vm_isolation]
      enabled = true
      nesting_host = "unix:///fleetci/nesting.sock"
      image = "class-\${FLEETCI_CLASS}"
      [runners.autoscaler.vm_isolation.connector_config]
        os = "linux"
        username = "fleetci"
        password = "unused"
        use_static_credentials = true
        timeout = "2m"
TOML
chmod 600 "$DATA/config.toml"
"$REPO/hack/ci-template.sh" fleetci 1vcpu-2gb 1vcpu-2gb 4vcpu-6gb > "$DATA/fleetci.yml"
chmod 644 "$DATA/fleetci.yml"

echo "ready: $DATA"
echo "  runner: gitlab-runner run --config $DATA/config.toml --working-directory $DATA/work"
echo "  CI include: copy $DATA/fleetci.yml to .gitlab/fleetci.yml in your project"
