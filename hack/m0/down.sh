#!/bin/sh
# Removes the M0 containers and network. Keeps keys and config.
set -u
docker ps -aq --filter label=fleetci.vm=1 | xargs -r docker rm -f >/dev/null
docker rm -f fleetci-m0-gateway >/dev/null 2>&1
docker network rm fleetci-m0 >/dev/null 2>&1
echo down
