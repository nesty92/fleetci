# The lab

A fleet to try fleetci on, on one machine: a controller and three workers,
each a cloud-hypervisor microVM in one container, and each worker on a
network of its own with a delay to the controller (eu +20 ms, us +80 ms,
ap +160 ms), as if it were somewhere else. The host needs KVM, nested.

    docker build -t fleetci-lab hack/lab
    docker run -d --init --name fleetci-lab --hostname lab \
      --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN \
      --sysctl net.ipv4.ip_forward=1 \
      -v $DATA:/data -v $IMAGES:/base:ro \
      -v $CLOUD_HYPERVISOR:/usr/local/bin/cloud-hypervisor:ro \
      -v $PWD/hack/lab:/lab:ro fleetci-lab sleep infinity
    docker exec fleetci-lab /lab/reset.sh

`$DATA` holds `bin/` (fleetci, fleeting-plugin-fleetingd,
cloud-hypervisor) and `secrets/` (`github-app.pem`, `github-app.id`,
`runner.token`); `$IMAGES` holds fleetingd's `prebuilt-*` and `published-*`
images, from a worker that has run a job, so the workers' first check
doesn't download them.

`reset.sh` boots the machines (`machines.sh`) and gives them fleetci, the
controller's config and the images (`provision.sh`). From there it is the
usual setup, on each machine through `docker exec fleetci-lab /data/ssh.sh
<ctl|eu|us|ap>`:

    ctl$ fleetci init && fleetci controller
    ctl$ fleetci token create            # one per worker; it prints the line below
    eu$  curl -fsSk --pinnedpubkey sha256//… https://ctl:9481/v1/install.sh |
           sh -s -- ctl <token> -labels region=eu -cpus 2 -memory 4GB

The line installs fleetci from the controller, joins, and starts the worker
as `fleetci-worker.service`. A machine with fleetci on it already can join
with `fleetci join -service -token <token> ctl`, or with `-foreground` to
watch the worker in the terminal.

`up.sh` does all of that with nobody typing, the controller as a systemd
unit that comes back when killed, and ends when the workers are Ready.

## Chaos

`chaos.py` runs on the host, with gh and glab logged in, against the fleet
`up.sh` brought up. It starts rounds of GitHub and GitLab jobs and, in
between, faults: a microVM killed, a worker or the controller killed or
stopped, a worker cut from the controller or from everything, a slow and
lossy network. A seed makes the whole plan up front, so a run is random and
can be run again:

    python3 -I hack/lab/chaos.py --minutes 10 $DATA/chaos-1
    python3 -I hack/lab/chaos.py --seed 42 --dry-run x     # just the plan
    python3 -I hack/lab/chaos.py --plan $DATA/chaos-1/plan.json $DATA/chaos-2

It heals everything after the last fault, or when stopped, waits for the
workers and the jobs, then checks that the workers are Ready, every job
ended, no worker went past its budget, no VM was left behind and every
failed job had a fault around it. The plan, what was done when, the
fleet's status every 3 s, the jobs, the logs and `report.md` go in the
directory; it exits 1 when a check failed.

`rewind.sh` takes the machines back to before `fleetci init`, keeping the
workers' images, in under a minute. The controller uses the GitLab runner
token in `secrets/`: stop any other fleetci serving the same one first.
