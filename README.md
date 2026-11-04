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
`.fleetci` alone gets the default class. Both kits write the include to
`fleetci.yml` next to the runner config ([hack/ci-template.sh](hack/ci-template.sh)).
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

## Setup

Write `fleetci.toml` (below), then let fleetci write gitlab-runner's side and
check the whole install:

```sh
fleetci runner-config -config /etc/fleetci/fleetci.toml -token-file runner.token \
  >> /etc/gitlab-runner/config.toml
fleetci check -config /etc/fleetci/fleetci.toml
```

`runner-config` prints the `[[runners]]` entry: every value in it follows from
`fleetci.toml`, so regenerate it rather than edit it. `check` changes nothing;
it reports, one line each:

- `fleetci.toml` loads and is valid;
- each class's plugin starts, its credentials work and its group exists;
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

Status: proof of concept, running locally. See [hack/m0](hack/m0/README.md)
to try it on one machine.
