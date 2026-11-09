#!/bin/sh
# A fleet on this machine, in containers, to try fleetci's controllers and
# workers end to end:
#
#   ctl-1, ctl-2   controllers; the lower id leads. They hold the forge
#                  credentials, on every network.
#   w1             worker on a network of its own ("site 1"), 2 cpus 4GB
#   w2             worker on another ("site 2"), 5 cpus 9GB
#   w3             FLEETCI_FLEET_W3=1: a worker without /dev/kvm, which the
#                  controllers turn away
#
# Each worker joins the way a real machine would: a token from ctl-1, then
# fleetci join, which installs the binaries from the controllers, writes
# worker.toml and checks a microVM per class before it is Ready.
#
# Jobs: GitHub, the WireZone App on the WireZone org (fleetci-playground{,-2});
# GitLab, ~/.config/fleetci/runner.token (FLEETCI_FLEET_GITLAB=1; stop
# fleetci-local first, it serves the same runner).
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DATA=${FLEETCI_FLEET_DATA:-$HOME/.local/share/fleetci-fleet}
CONF=$HOME/.config/fleetci
APP_ID_FILE=${FLEETCI_FLEET_APP_ID:-$CONF/github-app-wirezone.id}
APP_KEY=${FLEETCI_FLEET_APP_KEY:-$CONF/github-app-wirezone.pem}
# The App registers runners on the org: every WireZone repo is served.
ORG=${FLEETCI_FLEET_ORG:-WireZone}
RUNNER_TOKEN=${FLEETCI_FLEET_RUNNER_TOKEN:-$CONF/runner.token}
GITLAB=${FLEETCI_FLEET_GITLAB:-}
W3=${FLEETCI_FLEET_W3:-}
CH_VERSION=54.0
# fleetingd is our fork, built from its checkout next to fleetci.
MAIN=$(cd "$(git -C "$REPO" rev-parse --path-format=absolute --git-common-dir)/.." && pwd) # a worktree's main checkout
FD_SRC=${FLEETCI_FLEETINGD_SRC:-$MAIN/../fleeting-plugin-fleetingd}
IMAGE=fleetci-fleet-machine

[ -c /dev/kvm ] || { echo "no /dev/kvm" >&2; exit 1; }
[ -r "$APP_KEY" ] && [ -r "$APP_ID_FILE" ] || { echo "no GitHub App key or id in $CONF" >&2; exit 1; }
[ -z "$GITLAB" ] || [ -r "$RUNNER_TOKEN" ] || { echo "no runner token in $RUNNER_TOKEN" >&2; exit 1; }
[ -d "$FD_SRC/cmd/fleeting-plugin-fleetingd" ] || { echo "no fleetingd checkout at $FD_SRC (FLEETCI_FLEETINGD_SRC)" >&2; exit 1; }

umask 077
mkdir -p "$DATA/bin" "$DATA/artifacts"
(cd "$REPO" && CGO_ENABLED=0 go build -o "$DATA/bin/" ./cmd/fleetci)
# What joining workers install, served by the controllers. main.go embeds
# LICENSE and NOTICE, which upstream's CI puts there.
cp "$FD_SRC/LICENSE" "$FD_SRC/cmd/fleeting-plugin-fleetingd/"
[ -f "$FD_SRC/cmd/fleeting-plugin-fleetingd/NOTICE" ] || echo "local build" > "$FD_SRC/cmd/fleeting-plugin-fleetingd/NOTICE"
(cd "$FD_SRC" && CGO_ENABLED=0 go build -o "$DATA/artifacts/fleeting-plugin-fleetingd" ./cmd/fleeting-plugin-fleetingd)
[ -x "$DATA/artifacts/cloud-hypervisor" ] || {
  curl -fsSL -o "$DATA/artifacts/cloud-hypervisor" \
    "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v$CH_VERSION/cloud-hypervisor-static"
  chmod 755 "$DATA/artifacts/cloud-hypervisor"
}
chmod 755 "$DATA/bin" "$DATA/artifacts" "$DATA/bin/fleetci" "$DATA/artifacts/"*
docker build -q -t "$IMAGE" "$HERE" >/dev/null

for n in ctl w1 w2 w3; do
  docker network inspect "fleetci-fleet-$n" >/dev/null 2>&1 || docker network create "fleetci-fleet-$n" >/dev/null
done

gitlab=
[ -n "$GITLAB" ] && gitlab='
[gitlab]
url = "https://gitlab.com"

[[gitlab.runners]]
token_file = "/run/secrets/runner.token"'

# controller <id> <peer>
controller() {
  d=$DATA/$1
  mkdir -p "$d/etc" "$d/var"
  cat > "$d/etc/controller.toml" <<TOML
default_class = "1vcpu-2gb"

[controller]
id = "$1"
advertise = ["$1"]
peers = ["$2"]
artifacts = "/opt/fleetci/artifacts"

# The fleet's classes, defined here only; each worker runs those its
# resources fit, as many as its budget holds.
[classes.1vcpu-2gb]
cpus = 1
memory = "2GB"
disk = "20GB"

[classes.4vcpu-8gb]
cpus = 4
memory = "8GB"
disk = "30GB"

[github]
orgs = ["$ORG"]
app_id = $(cat "$APP_ID_FILE")
app_private_key_file = "/run/secrets/github-app.pem"
$gitlab
TOML
  docker rm -f "fleetci-fleet-$1" >/dev/null 2>&1 || true
  secrets="-v $APP_KEY:/run/secrets/github-app.pem:ro"
  [ -n "$GITLAB" ] && secrets="$secrets -v $RUNNER_TOKEN:/run/secrets/runner.token:ro"
  docker create --init --name "fleetci-fleet-$1" --hostname "$1" --network fleetci-fleet-ctl --network-alias "$1" \
    -v "$DATA/bin:/opt/fleetci/bin:ro" -v "$DATA/artifacts:/opt/fleetci/artifacts:ro" \
    -v "$d/etc:/etc/fleetci" -v "$d/var:/var/lib/fleetci" $secrets \
    --entrypoint /opt/fleetci/bin/fleetci "$IMAGE" controller >/dev/null
  for n in w1 w2 w3; do docker network connect --alias "$1" "fleetci-fleet-$n" "fleetci-fleet-$1"; done
}

controller ctl-1 ctl-2
# ctl-1 starts the fleet: its CA, then ctl-2 joins it as a controller.
docker run --rm -v "$DATA/bin:/opt/fleetci/bin:ro" -v "$DATA/ctl-1/etc:/etc/fleetci" -v "$DATA/ctl-1/var:/var/lib/fleetci" \
  --entrypoint /opt/fleetci/bin/fleetci "$IMAGE" init -config /etc/fleetci/controller.toml
docker start fleetci-fleet-ctl-1 >/dev/null
controller ctl-2 ctl-1
until docker exec fleetci-fleet-ctl-1 /opt/fleetci/bin/fleetci fleet status >/dev/null 2>&1; do sleep 1; done
if [ ! -f "$DATA/ctl-2/etc/pki/ca.key" ]; then
  tok=$(docker exec fleetci-fleet-ctl-1 /opt/fleetci/bin/fleetci token create -for-controller | head -1)
  docker run --rm --network fleetci-fleet-ctl -v "$DATA/bin:/opt/fleetci/bin:ro" -v "$DATA/ctl-2/etc:/etc/fleetci" \
    --entrypoint /opt/fleetci/bin/fleetci "$IMAGE" join -as-controller -id ctl-2 -advertise ctl-2 -token "$tok" ctl-1
fi
docker start fleetci-fleet-ctl-2 >/dev/null

# worker <id> <join flags…>: joins once, then runs the worker.
worker() {
  id=$1; shift
  d=$DATA/$id
  mkdir -p "$d/etc" "$d/var"
  docker rm -f "fleetci-fleet-$id" >/dev/null 2>&1 || true
  dev="--device /dev/kvm"
  [ "$id" = w3 ] && dev=
  if [ -f "$d/etc/worker.toml" ]; then
    cmd="worker"
  else
    tok=$(docker exec fleetci-fleet-ctl-1 /opt/fleetci/bin/fleetci token create | head -1)
    cmd="join -foreground -id $id -bin /var/lib/fleetci/bin -token $tok $* ctl-1"
  fi
  docker run -d --init --name "fleetci-fleet-$id" --hostname "$id" --network "fleetci-fleet-$id" \
    $dev --device /dev/net/tun --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1 \
    -v "$DATA/bin:/opt/fleetci/bin:ro" -v "$d/etc:/etc/fleetci" -v "$d/var:/var/lib/fleetci" \
    --entrypoint /opt/fleetci/bin/fleetci "$IMAGE" $cmd >/dev/null
}

worker w1 -cpus 2 -memory 4GB -labels site=1
worker w2 -cpus 5 -memory 9GB -labels site=2
[ -n "$W3" ] && worker w3 -cpus 2 -memory 4GB -labels site=3

cat <<MSG
fleet up. The workers check a microVM per class before they are Ready;
the first check downloads the image (a few minutes, once per worker).

  docker exec fleetci-fleet-ctl-1 /opt/fleetci/bin/fleetci fleet status
  docker logs -f fleetci-fleet-w1        # its join, then the worker
  docker logs -f fleetci-fleet-ctl-1     # the leader placing jobs
  gh workflow run basic.yml -R WireZone/fleetci-playground
  docker stop fleetci-fleet-ctl-1        # ctl-2 takes over in ~10 s
MSG
