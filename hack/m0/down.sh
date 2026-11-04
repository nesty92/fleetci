#!/bin/sh
# Removes leftover VMs and the network. Keeps the config.
set -u
docker ps -aq --filter label=fleetci.group | xargs -r docker rm -f -v >/dev/null
docker network rm fleetci-m0 >/dev/null 2>&1
echo down
