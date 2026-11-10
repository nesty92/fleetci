#!/bin/sh
# Runs in the lab: puts fleetci on the machines, the controller's config,
# credentials and the binaries workers install, and the job image on the
# workers so their first microVM check doesn't wait on a download.
set -eu
D=/data
S=$D/ssh.sh
cp() { scp -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i $D/id_ed25519 "$@"; }
ip() { case $1 in ctl) echo 10.20.0.10;; eu) echo 10.21.0.10;; us) echo 10.22.0.10;; ap) echo 10.23.0.10;; esac; }

for m in ctl eu us ap; do
  cp $D/bin/fleetci root@$(ip $m):/usr/local/bin/fleetci
  $S $m chmod 755 /usr/local/bin/fleetci
done

$S ctl 'mkdir -p /etc/fleetci/secrets /opt/fleetci/artifacts && chmod 700 /etc/fleetci/secrets'
cp $D/bin/fleeting-plugin-fleetingd $D/bin/cloud-hypervisor root@10.20.0.10:/opt/fleetci/artifacts/
cp $D/secrets/github-app.pem $D/secrets/runner.token root@10.20.0.10:/etc/fleetci/secrets/
$S ctl "chmod 600 /etc/fleetci/secrets/*; chmod 755 /opt/fleetci/artifacts/*; cat > /etc/fleetci/controller.toml" <<TOML
default_class = "1vcpu-2gb"

[controller]
id = "ctl"
advertise = ["ctl"]
artifacts = "/opt/fleetci/artifacts"
metrics = "0.0.0.0:9482"

[classes.1vcpu-2gb]
cpus = 1
memory = "2GB"
disk = "20GB"

[github]
orgs = ["WireZone"]
app_id = $(cat $D/secrets/github-app.id)
app_private_key_file = "/etc/fleetci/secrets/github-app.pem"

[gitlab]
url = "https://gitlab.com"

[[gitlab.runners]]
token_file = "/etc/fleetci/secrets/runner.token"
TOML

for m in eu us ap; do
  $S $m 'mkdir -p /var/lib/fleetci/images'
  cp /base/prebuilt-* /base/published-* root@$(ip $m):/var/lib/fleetci/images/ &
done
wait
echo provisioned
