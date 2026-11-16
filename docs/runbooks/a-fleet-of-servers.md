# A fleet of servers

Several servers, at one site or many, run jobs as one fleet. Controllers
take the jobs from GitHub and GitLab and place each on a worker with room;
the workers run them in microVMs. Only the controllers hold the forge
credentials: a worker never sees a token but the job's own.

```
GitHub, GitLab
   │  App / runner token
controller ctl-1 (leads) ── ctl-2 (standby, takes over in ~10 s)
   │  mTLS, :9481, the fleet's own CA
   ├── worker eu-1   fleetingd ─ Cloud Hypervisor microVMs
   ├── worker eu-2   …
   └── worker us-1   labels region=us
```

Needs fleetci 0.7.0 or later. Each worker needs `/dev/kvm`, systemd and
outbound HTTPS; it reaches the controllers on port 9481, nothing reaches
it. A controller needs no KVM and little else: a small VM will do.

## How placement works

- The classes are defined once, in the controllers' `controller.toml`.
  Each worker runs the classes its budget fits, as many VMs at once as its
  budget holds.
- A job goes to a worker with room: by default the fullest one that fits
  (`placement = "pack"`), or the emptiest (`"spread"`). A class with
  `on = { region = "us" }` goes only to workers with that label.
- A class with `cache_disk = true` goes, when it can, to a worker that
  already holds its project's caches ([Cache disks](../cache-disk.md#on-a-fleet-of-machines)).
- When no worker has room, the job goes to the class's `overflow`, another
  class of the fleet (say, one `on` other workers), or waits, in order.
- New jobs go only to workers the controllers hear from. A job whose
  worker stays unreachable for 10 minutes is given up.

## 1. The controller

Install fleetci, and put what joining workers install in one directory:

```sh
v=0.8.0 arch=linux-amd64 # x-release-please-version
base=https://github.com/nesty92/fleetci/releases/download/v$v
curl -fsSLO "$base/fleetci-$arch" -fsSLO "$base/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
install -m 755 "fleetci-$arch" /usr/local/bin/fleetci

mkdir -p /var/lib/fleetci/artifacts && cd /var/lib/fleetci/artifacts
curl -fsSL -o cloud-hypervisor \
  https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v54.0/cloud-hypervisor-static
curl -fsSL https://github.com/nesty92/fleeting-plugin-fleetingd/releases/download/v0.4.0-fleetci.4/fleeting-plugin-fleetingd_linux_amd64.tar.gz \
  | tar -xz fleeting-plugin-fleetingd
chmod 755 cloud-hypervisor fleeting-plugin-fleetingd
```

`/etc/fleetci/controller.toml`:

```toml
default_class = "4vcpu-8gb"

[controller]
id = "ctl-1"                       # the live controller with the lowest id leads
advertise = ["ctl-1.example.net"]  # where workers reach it; its certificate names these
peers = []                         # the other controllers, once there are some
artifacts = "/var/lib/fleetci/artifacts"
metrics = "127.0.0.1:9482"         # Prometheus, /metrics
# placement = "pack"               # or "spread"
# worker_update = "off"            # patch, minor, latest or v0.8.0: see Updates

[classes.2vcpu-4gb]
cpus = 2
memory = "4GB"

[classes.4vcpu-8gb]
cpus = 4
memory = "8GB"
disk = "30GB"
cache_disk = true                  # see Caches
# max_instances = 20               # across the fleet
# on = { region = "eu" }

[github]
orgs = ["your-org"]
app_id = 123456
app_private_key_file = "/etc/fleetci/github-app.pem"

[gitlab]
url = "https://gitlab.com"

[[gitlab.runners]]
token_file = "/etc/fleetci/runner.token"
```

`[github]` and `[gitlab]` are as with `fleetci serve` (the
[README](../../README.md#github-actions) has every key); either alone works.
The VMs boot fleetci's small image (Docker and actions/runner) unless a
class names another `image`.

Make the fleet's CA and the controller's certificate, then run it:

```sh
fleetci init -config /etc/fleetci/controller.toml   # the CA, in /etc/fleetci/pki; once, on the first controller
```

```ini
# /etc/systemd/system/fleetci-controller.service
[Unit]
Description=fleetci controller
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/bin/fleetci controller -config /etc/fleetci/controller.toml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload && systemctl enable --now fleetci-controller
```

Open port 9481 to the workers, and nothing else.

## 2. Workers

On the controller, one token per machine, good for an hour and one join:

```sh
fleetci token create
```

It prints the line to run on the new machine, as root. The line installs
fleetci from the controller, checks it by its sum, joins and starts
`fleetci-worker.service`:

```sh
curl -fsSk --pinnedpubkey sha256//… https://ctl-1.example.net:9481/v1/install.sh |
  sh -s -- ctl-1.example.net <token> -labels region=eu
```

`curl -k` is safe here: it checks the controller by the key it pins, not
by a CA. `fleetci join` flags go at the end:

| Flag | Default |
| --- | --- |
| `-labels region=eu,…` | none; matched by the classes' `on` |
| `-cpus 30`, `-memory 120GB` | all but the host's reserve |
| `-classes 4vcpu-8gb,…` | every class the budget fits |
| `-state /var/lib/fleetci` | where the VMs' disks, images and caches go |
| `-id eu-1` | the hostname |

The worker checks a microVM of each class before it is Ready; the first
check downloads the image, a few minutes once per worker. Its settings
are in `/etc/fleetci/worker.toml`, which `fleetci join` writes.

## 3. A second controller

Two or three controllers keep the fleet up when one goes. On the first:

```sh
fleetci token create -for-controller
```

On the new one, with fleetci installed and the same `controller.toml`
but its own `id` and `advertise`, and each controller's `peers` naming the
others:

```sh
fleetci join -as-controller -id ctl-2 -advertise ctl-2.example.net -token <token> ctl-1.example.net
systemctl enable --now fleetci-controller
```

The live controller with the lowest `id` leads. The others stand by and
take over within ten seconds of hearing nothing from it; jobs already
running go on. Workers that join afterwards learn every controller; a
worker that joined earlier needs the new one added to `controllers` in its
`worker.toml`.

## Caches

A class with `cache_disk = true` gives each job's VM a disk of its
project's caches, kept on the worker it ran on. Each worker sizes its own:

```toml
# /etc/fleetci/worker.toml
[cache_disk]
budget = "200GB"
```

Jobs go to a worker that holds their caches when it has room, waiting up
to `cache_wait` (10s) for one before they start from nothing elsewhere.
[Cache disks](../cache-disk.md) has the rest: who writes the caches, the
checkout cache, and GitHub's actions/cache.

## Updates

Workers can update themselves to the controllers' fleetci, one at a time,
each checked on trial and rolled back if its microVMs don't start:

```toml
# controller.toml
[controller]
worker_update = "patch"            # off, patch, minor, latest, or a version to pin

# worker.toml
[update]
auto = true
```

## 4. Check it

```sh
fleetci fleet status               # controllers, workers, classes, room
fleetci fleet status -checks       # and each worker's microVM checks
fleetci fleet jobs -n 20           # the last jobs, where they ran; -failed, -worker eu-1
```

Run a job of each class. The leader's log says `vm placed` per job.

## Operating it

| Task | How |
| --- | --- |
| Take a worker out | `fleetci fleet drain eu-1`: no new jobs; running ones finish. `undrain` to bring it back |
| Remove a worker for good | `fleetci fleet drain eu-1`, wait, `fleetci fleet revoke eu-1`; its certificate stops working |
| Run the checks again | `fleetci fleet verify [eu-1]` |
| Ask workers to update now | `fleetci fleet update-check` |
| A worker's log | `journalctl -u fleetci-worker` on it |
| Metrics | the leader's `metrics` address; [docs/grafana](../grafana) has a dashboard |

Not on a fleet yet: a shell into a failed job, and AWS, Google Cloud or
Azure groups for the spikes; a single server ([Bare metal with cloud spikes](bare-metal-with-cloud-spikes.md))
has both.
