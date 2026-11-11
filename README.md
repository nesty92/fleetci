# fleetci

Per-job machine classes for GitLab CI, on top of
[fleeting](https://gitlab.com/gitlab-org/fleeting). A job names the size it
needs; it gets a fresh VM of that size, used once.

<!-- x-release-please-start-version -->
```yaml
include:
  - remote: https://raw.githubusercontent.com/nesty92/fleetci/v0.7.1/templates/gitlab-ci.yml

build:
  extends: .fleetci-4vcpu-8gb    # the one line; drop it to go back
  image: golang:1.27
  services: [postgres:17]
  script: go build ./...
```
<!-- x-release-please-end -->

Stock gitlab-runner does the job as usual (`image:`, `services:`, cache,
artifacts). fleetci only decides where it runs: gitlab-runner loads
`fleeting-plugin-fleetci`, and its `vm_isolation` asks the plugin for a VM
named `class-${FLEETCI_CLASS}` once per job. Each class is its own fleeting
plugin, such as a cloud's, with its own warm VMs and limits.

Name classes after what they give, such as `2vcpu-4gb` or `8vcpu-32gb-arm`,
so a job's class says what it gets without opening the runner's config.

On AWS spot a Linux job costs about a tenth of a GitHub-hosted one; see
[docs/cost.md](docs/cost.md) for the comparison on 104M real jobs, network
and caches included.

## Picking a class on GitLab

[templates/gitlab-ci.yml](templates/gitlab-ci.yml) is a public template
with one hidden job per standard class: `1vcpu-2gb`, `2vcpu-4gb`,
`4vcpu-8gb`, `8vcpu-16gb` and `16vcpu-32gb`, each also with `-arm`.
`.fleetci` alone gets the runner's default class. Include it from a release
tag, as above. When your classes carry these names, that's all a project
needs.

Classes of your own go in a local file included after the template.
`fleetci ci-include -extra` writes one with only the classes the template
lacks:

<!-- x-release-please-start-version -->
```yaml
include:
  - remote: https://raw.githubusercontent.com/nesty92/fleetci/v0.7.1/templates/gitlab-ci.yml
  - local: .gitlab/fleetci.yml   # fleetci ci-include -extra > .gitlab/fleetci.yml
```
<!-- x-release-please-end -->

The same file can redefine one of the template's jobs, for example to send
`.fleetci-4vcpu-8gb` to a class of yours named otherwise: the later include
wins.

A job extending a class the runner lacks fails when it starts, with the
runner's classes in the error; `fleetci check` lists which of the
template's classes the runner has. Or skip the template:
`fleetci ci-include` writes every class of yours into one local file, and a
misspelled class then fails when the pipeline is created.

Each hidden job sets the runner tag `fleetci` and the variable
`FLEETCI_CLASS`. The variable is what carries the class, because GitLab
never passes a job's tags to the runner.

## GitHub Actions

The same classes serve GitHub Actions. Labels reach fleetci there, so the class
is the label:

```yaml
jobs:
  pr-title:
    runs-on: fleetci-1vcpu-2gb   # a PR title and description check needs little
  build:
    runs-on: fleetci-4vcpu-8gb   # class 4vcpu-8gb; runs-on: fleetci → default class
```

With a `[github]` table in the config, fleetci lists each repo's queued jobs,
takes a VM of the job's class from the same pool gitlab-runner uses, registers
a just-in-time runner with the job's labels and starts it on the VM over SSH.
The runner takes one job and exits; the VM is removed. Steps run on the VM
itself, as on GitHub's `ubuntu-latest`; `container:` and `services:` use its
Docker. GitLab jobs always run in a container on the VM.

A job asking for a class fleetci lacks, or for other labels besides
`self-hosted`, `linux` and its class's architecture (`X64`, `ARM64`), stays
queued and is logged. The VM image should carry actions/runner in
`~/actions-runner-<version>` (the images of [hack/local](hack/local) and the
[github image](images#github) do); otherwise it is downloaded for each job.
Without `runner_version`, fleetci runs the newest runner the image has.

### A whole organization

`orgs = ["acme"]` serves every repo of an organization, new ones included,
with runners registered on the org in `runner_group_id` (the org's default
group unless set; the group must allow the repos, and public ones only if
it allows public repos). A repo of a served org gets org runners even when
`repos` lists it too.

The token then needs *Self-hosted runners: Read and write* on the org, and
*Actions: Read* on its repos. Or sign in as a GitHub App instead of a
token: its tokens belong to no person, are renewed by fleetci every hour,
and each org it is installed on has its own rate limit.

```toml
[github]
orgs = ["acme"]
app_id = 123456
app_private_key_file = "github-app.pem"   # the key generated on the App's page
```

The App needs the same permissions (*Self-hosted runners*, *Actions: Read*,
and *Administration: Read and write* for repos listed in `repos`), and to
be installed on each org or repo.

### Webhooks

Without them fleetci finds jobs by listing each repo's queued runs every
10 seconds: at least two API calls per repo each time, so a token's 5,000
calls an hour last about seven idle repos, and an org's repos are listed
every 10 minutes to be polled too. With `[github.webhook]`, GitHub tells
fleetci when a job is queued, and it gets a VM at once:

```toml
[github.webhook]
listen = ":9480"
path = "/github"                 # default "/"
secret_file = "webhook.secret"   # the secret set on the webhook
# tls_cert_file, tls_key_file    # HTTPS; otherwise put it behind a proxy
```

Point a webhook at it, on the org, the repos or the App, sending
*Workflow jobs* events as `application/json`, with the same secret. fleetci
checks each delivery's signature and refuses the rest. Polling goes on
every 5 minutes (`poll_interval`) for the repos listed in `repos` and those
a delivery came from in the last day, so a delivery GitHub drops is served
late, not never. A job whose runner failed or took another job is looked at
again on its own.

### Without gitlab-runner

`fleetci serve -config fleetci.toml` runs the GitHub side as a process of its
own, for GitHub-only installs or to keep GitHub jobs going when gitlab-runner
or its fleetci plugin stops. Run it under a service manager that restarts it:

```ini
[Service]
ExecStart=/usr/bin/fleetci serve -config /etc/fleetci/github.toml
Restart=always
```

Its classes are its own. A class's group driven by two processes would have
each remove the other's VMs, so `serve` and gitlab-runner's plugin need
separate files with separate groups (on EC2 separate Auto Scaling groups),
and only `serve`'s has `[github]`. When `serve` stops it removes its VMs.

### Cache on your own storage

By default `actions/cache`, the caches built into `setup-node`, `setup-go`
and the like, and buildx's `type=gha` upload to GitHub, through your NAT, with
10 GB per repo. With `[github] cache_url` they go to a
[github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server)
instead, for example one on S3 in the VMs' region. No workflow changes. On
each VM fleetci patches the runner so it keeps the `ACTIONS_RESULTS_URL` it is
given, and points it at the server; the server passes everything that isn't
cache (artifacts) to GitHub. It speaks the v2 cache protocol, which
`actions/cache` v4 and later use.

Install `zstd` on every image. The cache key includes the compression,
so a VM without zstd (gzip) misses what a VM with it saved.
`hack/cache/up.sh` runs the server on RustFS for [hack/local](hack/local).

## Install

On AWS, [terraform-aws-gitlab-runner-autoscaler](https://github.com/nesty92/terraform-aws-gitlab-runner-autoscaler)
does all of this when fleetci is turned on for one of its runners. [docs/runbooks](docs/runbooks)
has whole setups: AWS with that module, your own servers with cloud spikes,
GitLab and GitHub from one runner, and GitHub only. By hand, from nothing to a first job:

**1. Binaries.** Each [release](https://github.com/nesty92/fleetci/releases)
has plain binaries for Linux and macOS (amd64, arm64) and a `checksums.txt`:

```sh
v=0.7.1 arch=linux-amd64 # x-release-please-version
base=https://github.com/nesty92/fleetci/releases/download/v$v
curl -fsSLO "$base/fleetci-$arch" -fsSLO "$base/fleeting-plugin-fleetci-$arch" -fsSLO "$base/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
install -m 755 "fleetci-$arch" /usr/local/bin/fleetci
install -m 755 "fleeting-plugin-fleetci-$arch" /usr/local/bin/fleeting-plugin-fleetci
```

Each class also needs its own fleeting plugin, on the PATH or by path in
`fleetci.toml`. `gitlab-runner fleeting install` only sees the plugins named
in `config.toml`, not fleetci's classes, so download it from its release,
such as [fleeting-plugin-aws](https://gitlab.com/gitlab-org/fleeting/plugins/aws/-/releases):

```sh
curl -fsSL -o /usr/local/bin/fleeting-plugin-aws \
  https://gitlab.com/gitlab-org/fleeting/plugins/aws/-/releases/v1.3.1/downloads/fleeting-plugin-aws-linux-amd64
chmod +x /usr/local/bin/fleeting-plugin-aws
```

**2. One instance group per class**, such as an Auto Scaling group set up as
its fleeting plugin asks (for AWS: min and desired 0, scale-in protection,
`AZRebalance` suspended). Nothing else may scale it. Its VM image needs:

- Docker, and an SSH user in the `docker` group (the class's
  `connector_config.username`);
- for GitHub Actions jobs, libicu and ideally actions/runner unpacked in
  `~/actions-runner-<version>`.

For fleetingd, the [images](images) have this, rebuilt weekly and on each
actions/runner release: `github` for GitHub Actions jobs, with the runner
and what workflows for GitHub's runners expect; `gitlab`, Docker alone, for
GitLab's.

fleetci waits for cloud-init to finish and for Docker to answer before it
hands a VM to a job. [hack/aws](hack/aws) has a working launch template and
boot script.

**3. A GitLab runner** registered with the tag `fleetci` and "Run untagged
jobs" off (project or group → Settings → CI/CD → Runners → New runner). Keep
its token in a file, such as `/etc/fleetci/runner.token`.

**4. `fleetci.toml`**, one class per group; see [Configuration](#configuration).

**5. gitlab-runner's side**, written by fleetci:

```sh
# a new config.toml, holding only this runner:
fleetci runner-config -global -token-file /etc/fleetci/runner.token > /etc/gitlab-runner/config.toml
# or beside other runners (then raise concurrent by the classes' max_instances):
fleetci runner-config -token-file /etc/fleetci/runner.token >> /etc/gitlab-runner/config.toml
```

**6. GitHub Actions (optional):** a fine-grained token for the repos, with
*Actions: Read* and *Administration: Read and write*, in a file named by
`[github] token_file`; for a whole org, see [A whole organization](#a-whole-organization).

**7. Check everything**, then start gitlab-runner:

```sh
fleetci check     # every class, GitHub access and config.toml; changes nothing
gitlab-runner run # or: systemctl restart gitlab-runner
```

**8. In each GitLab project**, include the public template and pick
classes with `extends:` (see [Picking a class on GitLab](#picking-a-class-on-gitlab)).
For classes the template lacks, also commit:

```sh
fleetci ci-include -extra > .gitlab/fleetci.yml
```

All three commands read `/etc/fleetci/fleetci.toml` unless given `-config`
or `$FLEETCI_CONFIG`.

### What `fleetci check` checks

It changes nothing, so it is safe while fleetci runs, and reports one line
each:

- `fleetci.toml` loads and is valid;
- each class's plugin starts, its credentials work and its group exists, and
  no two classes share a group;
- the host's budget, for the classes on this machine (see
  [Sharing this machine's CPUs and memory](#sharing-this-machines-cpus-and-memory));
- the GitHub token or App can list each repo's jobs and runners, and each
  org's repos and runners; the webhook's secret and certificate load;
- `config.toml` has fleetci's entry, naming each missing or wrong setting
  with the line to write.

It exits non-zero when anything fails.

## Configuration

gitlab-runner's `config.toml` only points at fleetci's own file
(`fleetci runner-config` writes this part):

```toml
[runners.autoscaler]
  plugin = "fleeting-plugin-fleetci"
  capacity_per_instance = 18  # GitLab jobs at once: the sum of the classes' limits
  max_instances = 1
  [runners.autoscaler.plugin_config]
    config_file = "/etc/fleetci/fleetci.toml"
  [runners.autoscaler.vm_isolation]
    enabled = true
    nesting_host = "unix:///fleetci/nesting.sock"
    image = "class-${FLEETCI_CLASS}"
```

```toml
# /etc/fleetci/fleetci.toml
default_class = "2vcpu-4gb"

[classes.1vcpu-2gb]            # linters, PR title and description checks
plugin = "fleeting-plugin-aws"
max_instances = 6
plugin_config = { name = "fleetci-1vcpu-2gb", region = "eu-north-1" }
connector_config = { username = "ubuntu", use_external_addr = true }

[classes.2vcpu-2gb]            # small checks
plugin = "fleeting-plugin-aws"
max_instances = 6
plugin_config = { name = "fleetci-2vcpu-2gb", region = "eu-north-1" }
connector_config = { username = "ubuntu", use_external_addr = true }

[classes.2vcpu-4gb]
plugin = "fleeting-plugin-aws"
max_instances = 4            # a ceiling; nothing runs until a job needs it
plugin_config = { name = "fleetci-2vcpu-4gb", region = "eu-north-1" }
connector_config = { username = "ubuntu", use_external_addr = true }

[classes.8vcpu-16gb]
plugin = "fleeting-plugin-aws"
max_instances = 2
plugin_config = { name = "fleetci-8vcpu-16gb", region = "eu-north-1" }
connector_config = { username = "ubuntu", use_external_addr = true }

# Warm VMs by time of day, as gitlab-runner's [[runners.autoscaler.policy]]:
# of the policies whose periods match, the last applies.
[[classes.8vcpu-16gb.policy]]
idle_count = 0                       # default: boot per job

[[classes.8vcpu-16gb.policy]]
periods = ["* 8-18 * * mon-fri"]
timezone = "Europe/Warsaw"
idle_count = 1                       # one warm in work hours…
scale_factor = 0.5                   # …or half the running jobs,
scale_factor_limit = 2               # at most 2
idle_time = "20m"

[github]
repos = ["owner/name"]
token_file = "github.token"          # fine-grained, Administration: write
cache_url = "http://10.0.1.5:3000/"  # optional: Actions cache server, see above
```

Each class needs an instance group of its own (an Auto Scaling group, say):
each class's scaler removes VMs in its group that it didn't start, so two
classes sharing one would remove each other's. fleetci refuses to start, and
`fleetci check` fails, when two classes name the same group. Nothing else
should scale those groups either, such as another runner.

A class without `policy` can set `idle_count` and `idle_time` directly, for
one policy at all times. Every class takes `acquire_timeout` (how long a job
waits for a VM, default 10m, or 50m with an `overflow`). Unknown keys are errors; relative paths are
relative to the file. Each class's `plugin_config` goes to its fleeting plugin
as it is.

### Reusing VMs

By default each VM runs one job and is removed. A class can reuse them, as
docker-autoscaler runners do, to keep pulled images and caches:
`capacity_per_instance` (jobs on a VM at once) and `max_use_count` (jobs
before it is removed), both 1 by default. `concurrent` grows by
`max_instances × capacity_per_instance`.

Reuse is for GitLab only, whose jobs run in containers on the VM. GitHub
steps run on the VM itself, as a user who can use its Docker, so a job could
read or change another job's VM. fleetci refuses to start, and
`fleetci check` fails, when a file with `[github]` has a class that reuses
VMs. To reuse VMs for GitLab and serve GitHub too, split them: GitHub in a
`fleetci serve` (or another runner) with single-use classes and groups of
their own.

### Your machines first, the cloud for spikes

`overflow` names a class that takes a class's jobs while all of its
`max_instances` are in use. For example, microVMs on your own server, with
EC2 for the rest:

```toml
[classes.4vcpu-8gb]
plugin = "fleeting-plugin-fleetingd"   # our fork, on this machine
max_instances = 6
overflow = "4vcpu-8gb-aws"
# plugin_config: vm_num_cpu_cores = 4, vm_memory_mb = 8192, …

[classes.4vcpu-8gb-aws]
plugin = "fleeting-plugin-aws"
max_instances = 20
plugin_config = { name = "fleetci-4vcpu-8gb", region = "eu-west-3" }
connector_config = { username = "ubuntu", use_external_addr = true }
```

Jobs keep asking for `4vcpu-8gb`, on GitLab and GitHub alike. The seventh
job running at once gets an EC2 VM, and once a job on the server ends, the
next one goes to the server again. When the overflow class is full too, the
job takes the first room in either, its own class first, for up to the
class's `acquire_timeout`, 50 minutes by default here. On GitLab the job
already shows as running while it waits, and the wait counts towards its job
timeout; on GitHub it stays queued.

The overflow class is a class like any other, so jobs can also ask for it by
name, but it can't have an overflow of its own.

### Spot VMs the cloud takes back

On AWS, fleetci reads each VM's instance metadata every 2 seconds, through
the SSH connection it already makes to the VM: nothing runs on the VM, and
no IAM is needed. A VM with a spot interruption notice takes no new job. Its
running jobs go on, since fleetci never stops a job for the cloud, and the
VM is removed once they end. The cloud takes the VM two minutes after the
notice, so a job still running then fails, as it would anyway.

```toml
[classes.4vcpu-8gb-aws.reclaim]
watch = true      # default for the AWS plugin; false turns it off
rebalance = true  # also act on rebalance recommendations; default false
```

A rebalance recommendation comes earlier and says the VM is at higher risk.
It is often not followed by an interruption, so by default it is only
logged. The VM's SSH server must allow TCP forwarding, as it does by
default.

### Sharing this machine's CPUs and memory

The classes whose VMs run on this machine share its CPUs and memory: by
default those of fleetingd and dockervm, with each VM's size read from
their `plugin_config`. A job gets a VM only while its size still fits, so
a big job and many small ones share the server without each class keeping
a worst-case share. `max_instances` stays each class's own ceiling.

The budget is detected at start: the CPUs and memory the machine has, or
its cgroup allows, less 1 CPU and a tenth of the memory (at least 2GB) for
the host. fleetci logs it, and `fleetci check` prints it with what the
classes could ask for at once. `[host]` overrides it:

```toml
[host]                                   # every key optional
reserve = { cpus = 4, memory = "16GB" }  # keep the detection, change the reserve
cpus    = 48                             # or set the budget outright; more than
memory  = "100GB"                        #   the machine over-commits on purpose
nominate_after = "10m"                   # see below; "0s" holds room for the oldest job at once
placement = "pack"                       # or "spread"; see below
enabled = false                          # no budget: each class keeps its max_instances

[classes.mine]
plugin = "/opt/my-local-plugin"          # a local plugin fleetci doesn't know:
host   = true                            #   put it on the budget, with its size
cpus   = 4
memory = "8GB"
```

A class can also set `host = false` to stay off the budget.

Waiting jobs are served as the Kubernetes scheduler serves pods. Room
that frees goes to the earliest waiting job it fits, and a job that doesn't
fit yet doesn't hold up later ones that do, so the server stays busy. A job
that has waited `nominate_after` (10 minutes) is *nominated*: from then on
the room that frees is held for it, and later jobs fill only what it
doesn't need, so small jobs can't starve a big one. One job is nominated at
a time, the one waiting longest; fleetci logs "job nominated". A job that
finds no room goes to its `overflow`, if it has one, instead of waiting.

When a class runs on more than one host with room, `placement` picks the
host: `pack` (the default) the busiest, so whole hosts stay free for large
jobs, or `spread` the idlest, so jobs have quieter neighbours. They are the
Kubernetes scheduler's MostAllocated and LeastAllocated scoring. Today
fleetci has one host, the machine it runs on, so this only matters once
classes run on more. Warm VMs (`idle_count`, or a policy's) and the VMs of a class that
reuses them hold their share all the time; fleetci refuses a budget they
don't fit, or one with no room left for a class's VM.
