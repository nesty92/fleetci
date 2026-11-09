#!/bin/sh
# Stops the fleet test kit. Its data (certificates, images) stays in
# ~/.local/share/fleetci-fleet; FLEETCI_FLEET_PURGE=1 removes that too.
set -eu
DATA=${FLEETCI_FLEET_DATA:-$HOME/.local/share/fleetci-fleet}
for c in w1 w2 w3 ctl-2 ctl-1; do docker rm -f "fleetci-fleet-$c" >/dev/null 2>&1 || true; done
for n in ctl w1 w2 w3; do docker network rm "fleetci-fleet-$n" >/dev/null 2>&1 || true; done
if [ "${FLEETCI_FLEET_PURGE:-}" = 1 ]; then
  # Root-owned files, written in the containers.
  docker run --rm -v "$DATA:/data" ubuntu:24.04 sh -c 'rm -rf /data/*'
  rmdir "$DATA" 2>/dev/null || true
fi
echo "fleet down"
