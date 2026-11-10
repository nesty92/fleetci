# The lab: one container that runs the fleet's machines as
# cloud-hypervisor microVMs, each region on a network of its own.
FROM ubuntu:24.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates cloud-image-utils iproute2 iptables \
      iputils-ping openssh-client qemu-utils \
 && rm -rf /var/lib/apt/lists/*
