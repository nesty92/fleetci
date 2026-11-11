#!/bin/sh
# Writes gitlab-runner's config.toml for the AWS test from `tofu output`:
# one fleetci class per Auto Scaling group, each run by fleeting-plugin-aws.
# The runner token is read from ~/.config/fleetci/runner.token and only
# written into the 0600 config.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DATA=${FLEETCI_AWS_DATA:-$HOME/.local/share/fleetci-aws}
TOKEN_FILE=${FLEETCI_LOCAL_TOKEN:-$HOME/.config/fleetci/runner.token}
AWS_PLUGIN_VERSION=${AWS_PLUGIN_VERSION:-v1.3.2}
# GitHub Actions on the same classes: space-separated owner/name repos, and a
# file holding a token that may register runners on them.
GITHUB_REPOS=${FLEETCI_GITHUB_REPOS:-}
GITHUB_TOKEN_FILE=${FLEETCI_GITHUB_TOKEN_FILE:-$HOME/.config/fleetci/github.token}

umask 077
mkdir -p "$DATA/work" "$DATA/bin"

(cd "$REPO" && go build -o "$DATA/bin/" ./cmd/fleeting-plugin-fleetci ./cmd/fleetci)
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
  "[classes.\(.key)]\n" +
  "plugin = \"\($bin)\"\n" +
  "max_instances = \($out_classes[.key])\n" +
  "acquire_timeout = \"10m\"\n" +
  "plugin_config = { name = \"\(.value)\", region = \"\($region)\"" +
  (if $profile != "" then ", profile = \"\($profile)\"" else "" end) + " }\n" +
  "connector_config = { username = \"ubuntu\", use_external_addr = true, timeout = \"5m\" }\n"
' --argjson out_classes "$(printf '%s' "$out" | jq '.classes.value')")

github=
if [ -n "$GITHUB_REPOS" ]; then
  [ -r "$GITHUB_TOKEN_FILE" ] || { echo "no GitHub token in $GITHUB_TOKEN_FILE" >&2; exit 1; }
  repos=$(printf '%s\n' $GITHUB_REPOS | jq -R . | jq -sc .)
  github="# GitHub Actions jobs on the same classes: runs-on: fleetci-<class>.
[github]
repos = $repos
token_file = \"$GITHUB_TOKEN_FILE\""
fi

# fleetci's own settings: one class per Auto Scaling group. Every VM boots
# for its job; add [[classes.<name>.policy]] to keep some warm.
cat > "$DATA/fleetci.toml" <<TOML
default_class = "$default"

$classes
$github
TOML

# gitlab-runner's side follows from fleetci.toml, so fleetci writes it.
"$DATA/bin/fleetci" runner-config -global -config "$DATA/fleetci.toml" \
  -name fleetci-aws -token-file "$TOKEN_FILE" -plugin "$DATA/bin/fleeting-plugin-fleetci" > "$DATA/config.toml"
chmod 600 "$DATA/config.toml" "$DATA/fleetci.toml"
"$DATA/bin/fleetci" ci-include -config "$DATA/fleetci.toml" > "$DATA/fleetci.yml"
chmod 644 "$DATA/fleetci.yml"

"$DATA/bin/fleetci" check -config "$DATA/fleetci.toml" -runner-config "$DATA/config.toml" ||
  echo "fleetci check found problems; see above" >&2

echo "ready: $DATA"
echo "  classes: $DATA/fleetci.toml"
echo "  runner: gitlab-runner run --config $DATA/config.toml --working-directory $DATA/work"
echo "  CI include: copy $DATA/fleetci.yml to .gitlab/fleetci.yml in your project"
