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
    ctl$ fleetci token create            # one per worker
    eu$  fleetci join -token <token> -labels region=eu -cpus 2 -memory 4GB -foreground ctl

`rewind.sh` takes the machines back to before `fleetci init`, keeping the
workers' images, in under a minute. The controller uses the GitLab runner
token in `secrets/`: stop any other fleetci serving the same one first.
