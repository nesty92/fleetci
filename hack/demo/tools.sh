#!/bin/sh
# Runs in the lab, after hack/lab's reset.sh or rewind.sh: what the recording uses on the
# machines. short, the log formatter; fleetwatch, the fleet's status every
# second; and a key on ctl, to hand each worker its join token.
set -eu
H=$(dirname "$0")
D=/data
S=$D/ssh.sh
cp() { scp -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i $D/id_ed25519 "$@"; }
for ip in 10.20.0.10 10.21.0.10 10.22.0.10 10.23.0.10; do
  cp $H/short.py root@$ip:/usr/local/bin/short
done
for m in ctl eu us ap; do $S $m 'chmod 755 /usr/local/bin/short; touch /root/.hushlogin'; done
cp $D/id_ed25519 root@10.20.0.10:/root/.ssh/id_ed25519
$S ctl 'cat > /root/.ssh/config' <<'CFG'
Host eu us ap
  User root
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
CFG
$S ctl 'cat > /usr/local/bin/fleetwatch; chmod 755 /usr/local/bin/fleetwatch' <<'W'
#!/bin/sh
# The fleet as the controller sees it, every second.
exec watch -t -n 1 'fleetci fleet status 2>/dev/null || echo "no controller yet"; echo; fleetci fleet jobs -n 8 2>/dev/null'
W
echo tools
