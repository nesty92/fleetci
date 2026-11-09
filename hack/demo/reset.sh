#!/bin/sh
# Runs in the lab: the machines from scratch, booted and provisioned.
set -eu
[ "${1:-}" = --wait ] || for m in ctl eu us ap; do [ -f /data/$m/pid ] && kill "$(cat /data/$m/pid)" 2>/dev/null || true; done
[ "${1:-}" = --wait ] || for m in ctl eu us ap; do
  while [ -f /data/$m/pid ] && kill -0 "$(cat /data/$m/pid)" 2>/dev/null; do sleep 0.5; done
  rm -rf /data/$m
done
[ "${1:-}" = --wait ] || /demo/machines.sh
end=$(($(date +%s) + 600))
ready() { for m in ctl eu us ap; do /data/ssh.sh $m test -e /run/demo-ready || return 1; done; }
until ready; do
  [ "$(date +%s)" -lt $end ] || { echo "machines did not come up" >&2; exit 1; }
  sleep 5
done
/demo/provision.sh
