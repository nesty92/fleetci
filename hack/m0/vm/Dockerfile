# A stand-in for a job VM: Ubuntu 24.04 with Docker and SSH, like GitHub's
# ubuntu-latest, so actions that expect it work. (actions/runner needs glibc,
# so not Alpine.)
FROM ubuntu:24.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg iptables openssh-server sudo libicu74 procps git \
 && install -d -m 0755 /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
 && echo "deb [signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io docker-buildx-plugin \
 && rm -rf /var/lib/apt/lists/* \
 && (userdel -r ubuntu 2>/dev/null || true) \
 && useradd -m -s /bin/bash -G docker fleetci \
 && usermod -p '*' fleetci \
 && mkdir -p /run/sshd
COPY --from=docker:29-dind /usr/local/bin/dind /usr/local/bin/dind
COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /usr/local/bin/fleetci-vm
# Docker's data lives on a volume: overlayfs can't stack on the VM's own overlay.
VOLUME /var/lib/docker
ENTRYPOINT ["/usr/local/bin/fleetci-vm"]
