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
# Optional Actions cache server (hack/cache/up.sh prints its URL).
GITHUB_CACHE_URL=${FLEETCI_GITHUB_CACHE_URL:-}

umask 077
mkdir -p "$DATA/work" "$DATA/bin"

(cd "$REPO" && go build -o "$DATA/bin/" ./cmd/fleeting-plugin-fleetci ./cmd/fleetci ./cmd/fleeting-plugin-dockervm)
docker build -q -t fleetci-m0-vm "$HERE/vm" >/dev/null
docker network inspect "$NET" >/dev/null 2>&1 || docker network create "$NET" >/dev/null

github=
if [ -n "$GITHUB_REPOS" ]; then
  [ -r "$GITHUB_TOKEN_FILE" ] || { echo "no GitHub token in $GITHUB_TOKEN_FILE" >&2; exit 1; }
  repos=$(printf '%s\n' $GITHUB_REPOS | jq -R . | jq -sc .)
  github="
# GitHub Actions jobs on the same classes: runs-on: fleetci-<class>.
[github]
repos = $repos
token_file = \"$GITHUB_TOKEN_FILE\""
  [ -n "$GITHUB_CACHE_URL" ] && github="$github
cache_url = \"$GITHUB_CACHE_URL\""
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

# gitlab-runner's side follows from fleetci.toml, so fleetci writes it.
"$DATA/bin/fleetci" runner-config -global -config "$DATA/fleetci.toml" \
  -name fleetci-m0 -token-file "$TOKEN_FILE" -plugin "$DATA/bin/fleeting-plugin-fleetci" > "$DATA/config.toml"
chmod 600 "$DATA/config.toml" "$DATA/fleetci.toml"
"$DATA/bin/fleetci" ci-include -config "$DATA/fleetci.toml" > "$DATA/fleetci.yml"
chmod 644 "$DATA/fleetci.yml"

"$DATA/bin/fleetci" check -config "$DATA/fleetci.toml" -runner-config "$DATA/config.toml" ||
  echo "fleetci check found problems; see above" >&2

echo "ready: $DATA"
echo "  classes: $DATA/fleetci.toml"
echo "  runner: gitlab-runner run --config $DATA/config.toml --working-directory $DATA/work"
echo "  CI include: copy $DATA/fleetci.yml to .gitlab/fleetci.yml in your project"
