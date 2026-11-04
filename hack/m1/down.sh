#!/bin/sh
# Stops the host container (its VMs, taps and rules go with it) and removes
# leftover dockervm VMs. Keeps the config and fleetingd's image cache.
set -u
docker rm -f fleetci-m1 >/dev/null 2>&1
docker ps -aq --filter label=fleetci.group | xargs -r docker rm -f -v >/dev/null
echo down
