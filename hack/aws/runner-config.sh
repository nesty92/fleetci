#!/bin/sh
# Writes gitlab-runner's config.toml for the AWS test from `tofu output`:
# one fleetci class per Auto Scaling group, each run by fleeting-plugin-aws.
# The runner token is read from ~/.config/fleetci/m0-runner.token and only
# written into the 0600 config.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DATA=${FLEETCI_AWS_DATA:-$HOME/.local/share/fleetci-aws}
TOKEN_FILE=${FLEETCI_M0_TOKEN:-$HOME/.config/fleetci/m0-runner.token}
AWS_PLUGIN_VERSION=${AWS_PLUGIN_VERSION:-v1.3.1}
# GitHub Actions on the same classes: space-separated owner/name repos, and a
# file holding a token that may register runners on them.
GITHUB_REPOS=${FLEETCI_GITHUB_REPOS:-}
GITHUB_TOKEN_FILE=${FLEETCI_GITHUB_TOKEN_FILE:-$HOME/.config/fleetci/github.token}

umask 077
mkdir -p "$DATA/work" "$DATA/bin"

(cd "$REPO" && go build -o "$DATA/bin/" ./cmd/fleeting-plugin-fleetci)
[ -x "$DATA/bin/fleeting-plugin-aws" ] ||
  GOBIN="$DATA/bin" go install "gitlab.com/gitlab-org/fleeting/plugins/aws/cmd/fleeting-plugin-aws@$AWS_PLUGIN_VERSION"

out=$(cd "$HERE" && tofu output -json)
region=$(printf '%s' "$out" | jq -r '.region.value')
profile=$(printf '%s' "$out" | jq -r '.profile.value // empty')
# Jobs without FLEETCI_CLASS get FLEETCI_DEFAULT_CLASS, else the first class
# by name.
default=${FLEETCI_DEFAULT_CLASS:-$(printf '%s' "$out" | jq -r '.groups.value | keys | first')}

classes=$(printf '%s' "$out" | jq -r --arg bin "$DATA/bin/fleeting-plugin-aws" --arg region "$region" --arg profile "$profile" '
  .groups.value | to_entries[] |
  "      [runners.autoscaler.plugin_config.classes.\(.key)]\n" +
  "        plugin = \"\($bin)\"\n" +
  "        max_instances = \($out_classes[.key])\n" +
  "        idle_count = 0\n" +
  "        acquire_timeout = \"10m\"\n" +
  "        [runners.autoscaler.plugin_config.classes.\(.key).plugin_config]\n" +
  "          name = \"\(.value)\"\n" +
  "          region = \"\($region)\"\n" +
  (if $profile != "" then "          profile = \"\($profile)\"\n" else "" end) +
  "        [runners.autoscaler.plugin_config.classes.\(.key).connector_config]\n" +
  "          username = \"ubuntu\"\n" +
  "          use_external_addr = true\n" +
  "          timeout = \"5m\""
' --argjson out_classes "$(printf '%s' "$out" | jq '.classes.value')")

github=
if [ -n "$GITHUB_REPOS" ]; then
  [ -r "$GITHUB_TOKEN_FILE" ] || { echo "no GitHub token in $GITHUB_TOKEN_FILE" >&2; exit 1; }
  repos=$(printf '%s\n' $GITHUB_REPOS | jq -R . | jq -sc .)
  github="      [runners.autoscaler.plugin_config.github]
        repos = $repos
        token_file = \"$GITHUB_TOKEN_FILE\""
fi

cat > "$DATA/config.toml" <<TOML
concurrent = 4
check_interval = 3
log_level = "info"

[[runners]]
  name = "fleetci-aws"
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
      default_class = "$default"
$classes
$github
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
        username = "ubuntu"
        password = "unused"
        use_static_credentials = true
        timeout = "5m"
TOML
chmod 600 "$DATA/config.toml"
# shellcheck disable=SC2046 # class names have no spaces
"$REPO/hack/ci-template.sh" fleetci "$default" $(printf '%s' "$out" | jq -r '.groups.value | keys[]') > "$DATA/fleetci.yml"
chmod 644 "$DATA/fleetci.yml"

echo "ready: $DATA"
echo "  runner: gitlab-runner run --config $DATA/config.toml --working-directory $DATA/work"
echo "  CI include: copy $DATA/fleetci.yml to .gitlab/fleetci.yml in your project"
