# The M1 host: gitlab-runner, fleetci, and fleetingd with Cloud Hypervisor,
# in one container. fleetingd's tap devices and nftables rules live in this
# container's network namespace, so the machine's own network is untouched.
FROM ubuntu:24.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git iproute2 nftables qemu-utils openssh-client \
 && rm -rf /var/lib/apt/lists/*
ARG CH_VERSION=54.0
ARG FLEETINGD_VERSION=0.3.4
RUN curl -fsSL -o /usr/local/bin/cloud-hypervisor \
      "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v$CH_VERSION/cloud-hypervisor-static" \
 && chmod 755 /usr/local/bin/cloud-hypervisor \
 && curl -fsSL "https://github.com/helmholtzcloud/fleeting-plugin-fleetingd/releases/download/v$FLEETINGD_VERSION/fleeting-plugin-fleetingd_linux_amd64.tar.gz" \
  | tar -xz -C /usr/local/bin fleeting-plugin-fleetingd \
 && curl -fsSL -o /usr/local/bin/gitlab-runner \
      "https://gitlab-runner-downloads.s3.amazonaws.com/latest/binaries/gitlab-runner-linux-amd64" \
 && chmod 755 /usr/local/bin/gitlab-runner
# The dockervm class drives the machine's Docker through its socket.
COPY --from=docker:29-cli /usr/local/bin/docker /usr/local/bin/docker
ENTRYPOINT ["gitlab-runner"]
