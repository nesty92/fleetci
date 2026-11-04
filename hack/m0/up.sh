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
# GitHub Actions on the same classes: space-separated owner/name repos, and a
# file holding a token that may register runners on them.
GITHUB_REPOS=${FLEETCI_GITHUB_REPOS:-}
GITHUB_TOKEN_FILE=${FLEETCI_GITHUB_TOKEN_FILE:-$HOME/.config/fleetci/github.token}

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
github=
if [ -n "$GITHUB_REPOS" ]; then
  [ -r "$GITHUB_TOKEN_FILE" ] || { echo "no GitHub token in $GITHUB_TOKEN_FILE" >&2; exit 1; }
  repos=$(printf '%s\n' $GITHUB_REPOS | jq -R . | jq -sc .)
  github="
# GitHub Actions jobs on the same classes: runs-on: fleetci-<class>.
[github]
repos = $repos
token_file = \"$GITHUB_TOKEN_FILE\""
fi

# fleetci's own settings. Every VM boots for its job (no warm VMs); see
# [[classes.<name>.policy]] in the README to keep some warm by time of day.
# The limits are ceilings: memory is only used by running VMs.
cat > "$DATA/fleetci.toml" <<TOML
default_class = "1vcpu-2gb"

[classes.1vcpu-2gb]
plugin = "$DATA/bin/fleeting-plugin-dockervm"
max_instances = 6
plugin_config = { name = "1vcpu-2gb", image = "fleetci-m0-vm", network = "$NET", cpus = "1", memory = "2g" }
connector_config = { username = "fleetci", timeout = "2m" }

[classes.4vcpu-6gb]
plugin = "$DATA/bin/fleeting-plugin-dockervm"
max_instances = 3
plugin_config = { name = "4vcpu-6gb", image = "fleetci-m0-vm", network = "$NET", cpus = "4", memory = "6g" }
connector_config = { username = "fleetci", timeout = "2m" }
$github
TOML

cat > "$DATA/config.toml" <<TOML
concurrent = 9
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
    # One gateway carries every GitLab job: up to the sum of the classes' limits.
    capacity_per_instance = 9
    max_use_count = 0
    max_instances = 1
    [runners.autoscaler.plugin_config]
      config_file = "$DATA/fleetci.toml"
    [runners.autoscaler.connector_config]
      timeout = "2m"
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
chmod 600 "$DATA/config.toml" "$DATA/fleetci.toml"
"$REPO/hack/ci-template.sh" fleetci 1vcpu-2gb 1vcpu-2gb 4vcpu-6gb > "$DATA/fleetci.yml"
chmod 644 "$DATA/fleetci.yml"

echo "ready: $DATA"
echo "  classes: $DATA/fleetci.toml"
echo "  runner: gitlab-runner run --config $DATA/config.toml --working-directory $DATA/work"
echo "  CI include: copy $DATA/fleetci.yml to .gitlab/fleetci.yml in your project"
