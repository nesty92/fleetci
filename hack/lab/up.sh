#!/bin/sh
# Runs in the lab, after reset.sh or rewind.sh: the fleet up with nobody
# typing. fleetci init on ctl, the controller as a systemd unit that comes
# back when killed, and each worker joined with the line `fleetci token
# create` prints. It ends when the three workers are Ready.
set -eu
S=/data/ssh.sh
# fleet <python expression of s, the status>: whether it holds.
fleet() { $S ctl "fleetci fleet status -json 2>/dev/null | python3 -c 'import json,sys; s=json.load(sys.stdin); sys.exit(not ($1))'"; }
until_ok() { # <seconds> <what> <fleet expression>
  end=$(($(date +%s) + $1))
  until fleet "$3" 2>/dev/null; do
    [ "$(date +%s)" -lt "$end" ] || { echo "gave up waiting: $2" >&2; exit 1; }
    sleep 2
  done
}

$S ctl 'test -d /etc/fleetci/pki || fleetci init >/root/init.log'
$S ctl 'systemctl is-active -q fleetci-controller ||
        { systemctl reset-failed fleetci-controller 2>/dev/null; systemd-run -q --unit=fleetci-controller \
            -p Restart=always -p RestartSec=2 /usr/local/bin/fleetci controller; }'
until_ok 60 'a leader' 's["leader"] == "ctl"'

pin=sha256//$($S ctl 'openssl x509 -in /etc/fleetci/pki/node.crt -pubkey -noout |
  openssl pkey -pubin -outform der | openssl dgst -sha256 -binary | base64')
for m in eu us ap; do
  # The token goes from ctl to the worker's file, and is printed nowhere.
  $S ctl 'fleetci token create -ttl 15m | head -1' | $S $m 'cat > token'
  $S $m "systemctl is-active -q fleetci-worker ||
         curl -fsSk --pinnedpubkey $pin https://ctl:9481/v1/install.sh |
         sh -s -- ctl \$(cat token) -labels region=$m -cpus 2 -memory 4GB >/root/join.log 2>&1" </dev/null &
done
wait
until_ok 900 'three Ready workers' 'sum(w["state"] == "Ready" for w in s["workers"]) >= 3'
echo up
