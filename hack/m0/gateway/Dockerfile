# The fleeting instance gitlab-runner connects to. It only forwards:
# to the nesting socket, and to job VMs on the private network.
FROM alpine:3.22
ARG UID=1000
RUN apk add --no-cache openssh-server \
 && ssh-keygen -A \
 && adduser -D -u "$UID" -s /bin/sh fleetci \
 && sed -i 's/^fleetci:!/fleetci:*/' /etc/shadow \
 && install -d -m 700 -o fleetci /home/fleetci/.ssh
COPY sshd_config /etc/ssh/sshd_config
CMD ["/usr/sbin/sshd", "-D", "-e"]
