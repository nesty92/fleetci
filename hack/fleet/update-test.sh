#!/bin/sh
# Workers updating themselves, on the kit (up.sh first), from releases
# signed with a test key and served on the workers' networks:
#
#   1. w1 and w2 run v0.1.0; v0.1.1 is released: both update to it, one at
#      a time, and commit it after their trial.
#   2. v0.1.2 is released with a cloud-hypervisor that does not start: the
#      first worker to take it fails its microVM check on trial, rolls back
#      to v0.1.1 and skips v0.1.2 from then on, and the leader stops the
#      rollout: the other worker stays on v0.1.1.
#
# The controllers get worker_update = "latest"; up.sh puts them back.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DATA=${FLEETCI_FLEET_DATA:-$HOME/.local/share/fleetci-fleet}
T=$DATA/update-test
SRV=$T/srv
IMAGE=fleetci-fleet-machine
TIMEOUT=${FLEETCI_UPDATE_TIMEOUT:-900}

docker inspect fleetci-fleet-ctl-1 >/dev/null 2>&1 || { echo "the kit is not up: hack/fleet/up.sh" >&2; exit 1; }
status() { docker exec fleetci-fleet-ctl-1 /opt/fleetci/bin/fleetci fleet status; }
# as root, in the kit's files
asroot() { docker run --rm -v "$DATA:/data" alpine:3.22 sh -c "$1"; }

rm -rf "$T" && mkdir -p "$SRV"
(cd "$REPO" && go run ./hack/release keygen -o "$T/test.key") | tail -1 > "$T/test.pub"
build() { # build <version>: a release's binaries, the kit's fleetingd and cloud-hypervisor
  d=$SRV/$1; mkdir -p "$d"
  (cd "$REPO" && CGO_ENABLED=0 go build -ldflags "-X main.version=$1" -o "$d/fleetci" ./cmd/fleetci)
  cp "$DATA/artifacts/fleeting-plugin-fleetingd" "$DATA/artifacts/cloud-hypervisor" "$d/"
}
sign() { (cd "$REPO" && go run ./hack/release local -version "$1" -dir "$SRV/$1" -url "http://releases/$1" -key "$T/test.key"); }
releases() { # releases <version>…: the list, as GitHub's API gives it
  printf '[' > "$SRV/releases"
  sep=
  for v in "$@"; do
    printf '%s{"tag_name":"%s","assets":[{"name":"versions.json","browser_download_url":"http://releases/%s/versions.json"}]}' "$sep" "$v" "$v" >> "$SRV/releases"
    sep=,
  done
  printf ']\n' >> "$SRV/releases"
}
build v0.1.0
build v0.1.1 && sign v0.1.1
build v0.1.2
printf '#!/bin/sh\necho "cloud-hypervisor: broken on purpose" >&2\nexit 1\n' > "$SRV/v0.1.2/cloud-hypervisor"
sign v0.1.2
releases v0.1.1
chmod -R a+rX "$T"

# The releases, at http://releases on the workers' networks.
docker rm -f fleetci-fleet-releases >/dev/null 2>&1 || true
docker run -d --name fleetci-fleet-releases --network fleetci-fleet-w1 --network-alias releases \
  -v "$SRV:/www:ro" busybox httpd -f -p 80 -h /www >/dev/null
docker network connect --alias releases fleetci-fleet-w2 fleetci-fleet-releases

# The controllers: this checkout's fleetci, letting the workers take the
# latest.
sh "$REPO/hack/build-l1.sh"
(cd "$REPO" && CGO_ENABLED=0 go build -o "$DATA/bin/" ./cmd/fleetci)
for c in ctl-1 ctl-2; do
  asroot "grep -q '^worker_update' /data/$c/etc/controller.toml || sed -i 's/^\[controller\]\$/[controller]\nworker_update = \"latest\"/' /data/$c/etc/controller.toml"
  docker restart "fleetci-fleet-$c" >/dev/null
done

# The workers: v0.1.0, run from their state dir, which the update writes.
pub=$(cat "$T/test.pub")
for w in w1 w2; do
  asroot "cp /data/update-test/srv/v0.1.0/fleetci /data/$w/var/bin/fleetci && chmod 755 /data/$w/var/bin/fleetci &&
    rm -rf /data/$w/var/update /data/$w/var/update.trial &&
    sed -i '/^\[update\]/,\$d' /data/$w/etc/worker.toml &&
    printf '\n[update]\n  auto = true\n  source = \"http://releases/releases\"\n  public_key = \"$pub\"\n  check_every = \"10m\"\n  trial = \"30s\"\n' >> /data/$w/etc/worker.toml"
  docker rm -f "fleetci-fleet-$w" >/dev/null
  docker run -d --init --name "fleetci-fleet-$w" --hostname "$w" --network "fleetci-fleet-$w" \
    --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1 \
    -v "$DATA/bin:/opt/fleetci/bin:ro" -v "$DATA/$w/etc:/etc/fleetci" -v "$DATA/$w/var:/var/lib/fleetci" \
    --entrypoint /var/lib/fleetci/bin/fleetci "$IMAGE" worker >/dev/null
done

# wait <what> <shell test on $s, the status>
wait_for() {
  start=$(date +%s)
  until s=$(status 2>/dev/null) && eval "$2"; do
    [ $(($(date +%s) - start)) -lt "$TIMEOUT" ] || { echo "timed out: $1" >&2; status >&2 || true; exit 1; }
    sleep 5
  done
  echo "ok    $1 ($(($(date +%s) - start))s)"
}
ready() { echo "$s" | grep -Eq "^$1 +$2 +Ready "; }

wait_for "w1 and w2 Ready on v0.1.0" 'ready w1 v0.1.0 && ready w2 v0.1.0'
docker exec fleetci-fleet-ctl-1 /opt/fleetci/bin/fleetci fleet update-check
wait_for "w1 and w2 on v0.1.1" 'ready w1 v0.1.1 && ready w2 v0.1.1'
wait_for "their trials over" '! echo "$s" | grep -q "update trial"'
for w in w1 w2; do
  asroot "test ! -e /data/$w/var/update.trial" || { echo "$w: still on trial" >&2; exit 1; }
done
echo "ok    both committed v0.1.1"

releases v0.1.1 v0.1.2
docker exec fleetci-fleet-ctl-1 /opt/fleetci/bin/fleetci fleet update-check
# Its VM never comes up: the check fails after 10 minutes, and the trial
# with it.
TIMEOUT=$((TIMEOUT + 600))
wait_for "a worker rolled back from v0.1.2" 'echo "$s" | grep -q "update failed v0.1.2"'
wait_for "w1 and w2 Ready on v0.1.1 again" 'ready w1 v0.1.1 && ready w2 v0.1.1'
failed=0
for w in w1 w2; do
  asroot "grep -q v0.1.2 /data/$w/var/update/failed.json 2>/dev/null" && failed=$((failed + 1))
done
[ "$failed" = 1 ] || { echo "$failed workers tried v0.1.2; the rollout should stop at 1" >&2; exit 1; }
echo "ok    one worker tried v0.1.2, the rollout stopped"
status
echo "update test passed. The workers stay on v0.1.1; hack/fleet/up.sh puts the kit back."
