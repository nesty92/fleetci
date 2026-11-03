#!/bin/sh
# Prepares the M0 proof on this machine: images, network, keys, the gateway
# container and gitlab-runner's config.toml. The runner token is read from
# ~/.config/fleetci/m0-runner.token and only written into the 0600 config.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DATA=${FLEETCI_M0_DATA:-$HOME/.local/share/fleetci-m0}
TOKEN_FILE=${FLEETCI_M0_TOKEN:-$HOME/.config/fleetci/m0-runner.token}
NET=fleetci-m0

umask 077
mkdir -p "$DATA/run" "$DATA/work" "$DATA/bin"

(cd "$REPO" && go build -o "$DATA/bin/" ./cmd/fleetci-nesting ./cmd/fleeting-plugin-static)

docker build -q -t fleetci-m0-vm "$HERE/vm" >/dev/null
docker build -q -t fleetci-m0-gateway --build-arg UID="$(id -u)" "$HERE/gateway" >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

[ -f "$DATA/gateway.key" ] || ssh-keygen -q -t ed25519 -N '' -C fleetci-m0 -f "$DATA/gateway.key"
[ -f "$DATA/vm-password" ] || head -c 24 /dev/urandom | base64 | tr -d '/+=' > "$DATA/vm-password"
chmod 644 "$DATA/gateway.key.pub"
chmod 755 "$DATA" "$DATA/run"

docker rm -f fleetci-m0-gateway >/dev/null 2>&1 || true
docker run -d --name fleetci-m0-gateway --network "$NET" \
  -p 127.0.0.1:2222:22 \
  -v "$DATA/run:/run/fleetci" \
  -v "$DATA/gateway.key.pub:/home/fleetci/.ssh/authorized_keys:ro" \
  fleetci-m0-gateway >/dev/null

# gitlab-runner 19.5 dials with a zero timeout when connector_config has
# none, which fails every SSH attempt at once. Both connectors set it.
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
    plugin = "$DATA/bin/fleeting-plugin-static"
    capacity_per_instance = 4
    max_use_count = 0
    max_instances = 1
    [runners.autoscaler.plugin_config]
      address = "127.0.0.1:2222"
      name = "m0-gateway"
    [runners.autoscaler.connector_config]
      os = "linux"
      arch = "amd64"
      username = "fleetci"
      key_path = "$DATA/gateway.key"
      use_static_credentials = true
      timeout = "2m"
    [[runners.autoscaler.policy]]
      idle_count = 1
      idle_time = "30m"
    [runners.autoscaler.vm_isolation]
      enabled = true
      nesting_host = "unix:///run/fleetci/nesting.sock"
      image = "class-\${FLEETCI_CLASS}"
      [runners.autoscaler.vm_isolation.nesting_config]
        source = "fleetci-m0"
      [runners.autoscaler.vm_isolation.connector_config]
        os = "linux"
        arch = "amd64"
        username = "fleetci"
        password = "$(cat "$DATA/vm-password")"
        use_static_credentials = true
        timeout = "2m"
TOML
chmod 600 "$DATA/config.toml"

echo "ready: $DATA"
echo "  daemon: NESTING_SOCKET=$DATA/run/nesting.sock $DATA/bin/fleetci-nesting -config $HERE/nesting.json -password-file $DATA/vm-password"
echo "  runner: gitlab-runner run --config $DATA/config.toml --working-directory $DATA/work"
