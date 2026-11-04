# fleetci

Per-job machine classes for GitLab CI, on top of
[fleeting](https://gitlab.com/gitlab-org/fleeting). A job names the size it
needs; it gets a fresh VM of that size, used once.

```yaml
include:
  - local: .gitlab/fleetci.yml   # one hidden job per class

build:
  extends: .fleetci-4vcpu-8gb    # the one line; drop it to go back
  image: golang:1.27
  services: [postgres:17]
  script: go build ./...
```

Stock gitlab-runner does the job as usual (`image:`, `services:`, cache,
artifacts). fleetci only decides where it runs: gitlab-runner loads
`fleeting-plugin-fleetci`, and its `vm_isolation` asks the plugin for a VM
named `class-${FLEETCI_CLASS}` once per job. Each class is its own fleeting
plugin, such as a cloud's, with its own warm VMs and limits.

Name classes after what they give, such as `2vcpu-4gb` or `8vcpu-32gb-arm`,
so a job's class says what it gets without opening the runner's config.

Each `.fleetci-<class>` sets the runner tag and `FLEETCI_CLASS` together;
`.fleetci` alone gets the default class. `fleetci ci-include` writes the
include from `fleetci.toml`.
A misspelled class fails when the pipeline is created
(`unknown keys in extends`), before any VM boots. GitLab does not pass a job's
tags to the runner, so the class can't be a tag alone.

## GitHub Actions

The same classes serve GitHub Actions. Labels reach fleetci there, so the class
is the label:

```yaml
jobs:
  build:
    runs-on: fleetci-4vcpu-8gb   # class 4vcpu-8gb; runs-on: fleetci → default class
```

With a `[github]` table in the config, fleetci lists each repo's queued jobs,
takes a VM of the job's class from the same pool gitlab-runner uses, registers
a just-in-time runner with the job's labels and starts it on the VM over SSH.
The runner takes one job and exits; the VM is removed.

A job asking for a class fleetci lacks, or for other labels besides
`self-hosted` and `linux`, stays queued and is logged. The VM image should
carry actions/runner in `~/actions-runner-<version>` (both kits do); otherwise
it is downloaded for each job.

## Install

On AWS, [terraform-aws-gitlab-runner-autoscaler](https://github.com/nesty92/terraform-aws-gitlab-runner-autoscaler)
does all of this when given a `fleetci` input. By hand, from nothing to a
first job:

**1. Binaries.** Each [release](https://github.com/nesty92/fleetci/releases)
has plain binaries for Linux and macOS (amd64, arm64) and a `checksums.txt`:

```sh
v=0.1.0 arch=linux-amd64
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
`[github] token_file`.

**7. Check everything**, then start gitlab-runner:

```sh
fleetci check     # every class, GitHub access and config.toml; changes nothing
gitlab-runner run # or: systemctl restart gitlab-runner
```

**8. In each GitLab project**, commit the include and pick classes with
`extends:`:

```sh
fleetci ci-include > .gitlab/fleetci.yml
```

All three commands read `/etc/fleetci/fleetci.toml` unless given `-config`
or `$FLEETCI_CONFIG`.

### What `fleetci check` checks

It changes nothing, so it is safe while fleetci runs, and reports one line
each:

- `fleetci.toml` loads and is valid;
- each class's plugin starts, its credentials work and its group exists, and
  no two classes share a group;
- the GitHub token can list each repo's jobs and runners;
- `config.toml` has fleetci's entry, naming each missing or wrong setting
  with the line to write.

It exits non-zero when anything fails.

## Configuration

gitlab-runner's `config.toml` only points at fleetci's own file
(`fleetci runner-config` writes this part):

```toml
[runners.autoscaler]
  plugin = "fleeting-plugin-fleetci"
  capacity_per_instance = 8   # GitLab jobs at once: the sum of the classes' limits
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
```

Each class needs an instance group of its own (an Auto Scaling group, say):
each class's scaler removes VMs in its group that it didn't start, so two
classes sharing one would remove each other's. fleetci refuses to start, and
`fleetci check` fails, when two classes name the same group. Nothing else
should scale those groups either, such as another runner.

A class without `policy` can set `idle_count` and `idle_time` directly, for
one policy at all times. Every class takes `acquire_timeout` (how long a job
waits for a VM, default 10m). Unknown keys are errors; relative paths are
relative to the file. Each class's `plugin_config` goes to its fleeting plugin
as it is.
