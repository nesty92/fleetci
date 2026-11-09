# A machine of the fleet test kit: a bare Ubuntu with what fleetci join
# checks for. fleetci join installs the rest (cloud-hypervisor,
# fleeting-plugin-fleetingd) from the controllers into /var/lib/fleetci/bin.
FROM ubuntu:24.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl iproute2 nftables qemu-utils \
 && rm -rf /var/lib/apt/lists/*
ENV PATH=/var/lib/fleetci/bin:$PATH
