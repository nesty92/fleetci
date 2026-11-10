#!/bin/sh
# Runs in the lab: the machines from scratch, booted and provisioned.
# With --wait, only waits for machines already booting, then provisions.
set -eu
if [ "${1:-}" != --wait ]; then
  for m in ctl eu us ap; do [ -f /data/$m/pid ] && kill "$(cat /data/$m/pid)" 2>/dev/null || true; done
  for m in ctl eu us ap; do
    while [ -f /data/$m/pid ] && kill -0 "$(cat /data/$m/pid)" 2>/dev/null; do sleep 0.5; done
    rm -rf /data/$m
  done
  /lab/machines.sh
fi
end=$(($(date +%s) + 600))
ready() { for m in ctl eu us ap; do /data/ssh.sh $m test -e /run/lab-ready || return 1; done; }
until ready; do
  [ "$(date +%s)" -lt $end ] || { echo "machines did not come up" >&2; exit 1; }
  sleep 5
done
/lab/provision.sh
