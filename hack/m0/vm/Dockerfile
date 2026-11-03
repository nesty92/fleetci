# A stand-in for a job VM: Docker plus SSH, nothing else.
FROM docker:29-dind
RUN apk add --no-cache openssh-server \
 && (addgroup -S docker 2>/dev/null || true) \
 && adduser -D -s /bin/sh fleetci \
 && sed -i 's/^fleetci:!/fleetci:*/' /etc/shadow \
 && addgroup fleetci docker
COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /usr/local/bin/fleetci-vm
ENTRYPOINT ["/usr/local/bin/fleetci-vm"]
