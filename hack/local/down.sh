#!/bin/sh
# Stops the kit's container (its microVMs, taps and rules go with it),
# removes leftover container VMs and the network. Keeps the config and
# fleetingd's image cache.
set -u
docker rm -f fleetci-local >/dev/null 2>&1
docker ps -aq --filter label=fleetci.group | xargs -r docker rm -f -v >/dev/null
docker network rm fleetci-local >/dev/null 2>&1
echo down
