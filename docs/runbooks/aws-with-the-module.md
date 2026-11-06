# AWS with the terraform module

[terraform-aws-gitlab-runner-autoscaler](https://github.com/nesty92/terraform-aws-gitlab-runner-autoscaler)
runs gitlab-runner on a Runner Manager instance: the main runners
(`runner-amd64`, `runner-arm64`) and any runner pools, each on its own Auto
Scaling group. fleetci is turned on for a runner you already have, and
changes one thing: that runner gets its VMs through `fleeting-plugin-fleetci`
instead of `fleeting-plugin-aws`. It keeps its registration, token, tags,
docker options and cache; nothing new is registered. Its group becomes one
class, the default, and each extra class gets a group of its own.

```
Runner Manager
  gitlab-runner
    runner-amd64   fleeting-plugin-fleetci-amd64 → gitlab-runner-amd64-asg             class 2vcpu-4gb (default)
                                                 → gitlab-runner-amd64-1vcpu-2gb-asg
                                                 → gitlab-runner-amd64-8vcpu-16gb-asg
                   [github] owner/api, owner/web → the same classes
    runner-ci      fleeting-plugin-aws           → gitlab-runner-ci-asg                (a pool without fleetci: unchanged)
```

The module writes `/etc/fleetci/amd64.toml`, links the plugin as
`/usr/bin/fleeting-plugin-fleetci-amd64`, runs `fleetci check` and starts
`fleetci-watchdog@amd64`. Nothing is installed by hand.

| Runner | Input | Its own class takes |
| --- | --- | --- |
| `runner-amd64` | `runner_fleetci_amd64` | `runner_instance_amd64`, `runner_autoscaler_options_amd64`, `runner_autoscaler_policy_amd64` |
| `runner-arm64` | `runner_fleetci_arm64` | the same `_arm64` inputs |
| `runner-<key>`, a pool | `runner_pools.<key>.fleetci` | the pool's `ami_id`, `instance`, `autoscaler`, `policy` |

All three take the same block. The examples below use `runner-amd64`; for
another runner, the paths and names use its key (`arm64`, or the pool's).

## 1. The image

A runner with fleetci, and each of its classes, boots a fleetci image: Ubuntu
24.04 with Docker and actions/runner, with job containers kept away from
instance metadata. The module builds it with Packer, one AMI per
architecture:

```sh
cd packer/fleetci
packer init .
packer build -var="vpc_id=vpc-…" -var="subnet_id=subnet-…" -var="region=eu-north-1" .
```

Every class of an architecture can share its AMI.

## 2. Tokens

- **GitLab:** nothing new to register. Add the tag `fleetci` to the
  runner's existing registration in GitLab. Keep "Run untagged jobs" as you
  had it: untagged jobs go to the default class.
- **GitHub** (optional): a fine-grained token for the repos with
  *Actions: Read* and *Administration: Read and write*, in an SSM
  SecureString.

## 3. Turn it on

```hcl
module "runners" {
  source = "github.com/nesty92/terraform-aws-gitlab-runner-autoscaler?ref=3.9.0"
  # … VPC, subnets, the Manager, the token: as you have them

  runner_instance_amd64 = {
    ami_id                  = var.fleetci_ami_amd64          # was the docker-autoscaler AMI
    type                    = "c7a.large"
    instance_type_overrides = [{ instance_type = "c7i.large" }]
  }
  runner_autoscaler_options_amd64 = { max_instances = 4, capacity_per_instance = 1, max_use_count = 1 } # one job per VM, for github

  runner_fleetci_amd64 = {
    class = "2vcpu-4gb"            # runner-amd64's own group
    classes = {
      "1vcpu-2gb"  = { instance_types = ["c7a.medium"], max_instances = 6, ami_id = var.fleetci_ami_amd64 }
      "8vcpu-16gb" = { instance_types = ["c7i.2xlarge", "c6i.2xlarge"], max_instances = 2, ami_id = var.fleetci_ami_amd64 }
    }
    github = {                     # optional
      repos = ["owner/api", "owner/web"]
      token = { name = "fleetci-github-token", arn = aws_ssm_parameter.gh.arn, region = "eu-north-1" }
    }
  }
}
```

On a pool, the same block goes in `runner_pools.<key>.fleetci`, and the
pool's `ami_id` becomes the fleetci image.

| Key | Default | |
| --- | --- | --- |
| `class` | | the runner's own class; its size, reuse and warm VMs come from the runner's instance, autoscaler and policy inputs (table above) |
| `acquire_timeout` | `10m` | how long a GitLab job waits for a VM of the runner's class |
| `classes.<name>` | | `instance_types` (several make a mixed instances policy), `ami_id`, `architecture` (`amd64`), `max_instances` (4), `capacity_per_instance` and `max_use_count` (1), `policy` (none: every VM boots for its job), `user_data`, `username` (`ubuntu`), `volume_size` (30), `subnet_ids`, `private_address_only` (true), `spot_allocation_strategy`, `acquire_timeout` |
| `github` | | `repos` and the SSM parameter of the GitHub token |
| `github_runner_version` | `2.338.2` | keep it equal to the image's `runner_version`, or each GitHub job downloads the runner |
| `watchdog` | `true` | restarts gitlab-runner when the runner's plugin has been gone for 30 s |

`fleetci_version` (a module input) picks the fleetci release. Class names
must match `^[a-z0-9][a-z0-9-]{0,29}$`. Prefer non-burstable types: a T type
bills surplus CPU credits for a CPU-bound job.

`tofu apply` replaces the Manager, as any runner change does. The
runner's group stays; only the Manager's config changes.

## 4. Reusing VMs

A class may keep a VM for several jobs, to keep pulled images and caches:
`capacity_per_instance` and `max_use_count` on an extra class, the runner's
autoscaler options for its own class. docker-autoscaler's default
`max_use_count` is 10, so a runner reuses VMs unless it says otherwise.

That is for GitLab only. GitHub steps run on the VM itself, so a job could
read or change another job's VM, and the plan stops, naming the runner, when
a runner with `github` has a class that reuses VMs. To reuse VMs for GitLab
and serve GitHub too, use two runners: say the main runner keeps reusing,
and a pool serves GitHub single-use.

```hcl
runner_fleetci_amd64 = { class = "2vcpu-4gb" }   # runner-amd64 reuses VMs: max_use_count 10

runner_pools = {
  github = {                                     # runner-github: one job per VM, serves GitHub
    architecture = "amd64"
    ami_id       = var.fleetci_ami_amd64
    token        = { … }                         # a GitLab registration of its own, tag fleetci-github
    autoscaler   = { max_use_count = 1 }
    fleetci = {
      class  = "2vcpu-4gb"
      github = { repos = ["owner/api"], token = { … } }
    }
  }
}
```

The pool is the one new runner here, and only because the two must not
share VMs. Give its GitLab registration a tag of its own, or no GitLab jobs
at all, so GitLab jobs keep going to the reusing runner. Each runner has
its own fleetci and groups. A GitHub repo can be served by one runner only.
Single-use classes are fine for both forges on one runner: every VM is
removed after its job.

## 5. The workflows

<!-- x-release-please-start-version -->
```yaml
# GitLab
include:
  - remote: https://raw.githubusercontent.com/nesty92/fleetci/v0.5.0/templates/gitlab-ci.yml

lint:
  extends: .fleetci-1vcpu-2gb
build:
  extends: .fleetci-8vcpu-16gb   # or .fleetci for the default class
```
<!-- x-release-please-end -->

```yaml
# GitHub
jobs:
  build:
    runs-on: fleetci-8vcpu-16gb  # or fleetci for the default class
```

The public template covers the standard ladder (1vcpu-2gb to 16vcpu-32gb,
each also `-arm`). For other class names, commit the runner's `ci_include`:

```sh
tofu output -json fleetci | jq -r .amd64.ci_include > .gitlab/fleetci.yml
```

`tofu output fleetci` also lists each class's Auto Scaling group and GitHub
label.

## 6. Check it

On the Manager (SSH, or Session Manager with `runner_manager.enable_ssm_agent = true`):

```sh
grep -E '^(ok|FAIL)  ' /var/log/cloud-init-output.log   # what fleetci check found at boot
systemctl status gitlab-runner fleetci-watchdog@amd64
journalctl -u gitlab-runner -f
```

`fleetci check` ran at boot for each runner with fleetci, one line per part:

```
ok    fleetci.toml                     /etc/fleetci/amd64.toml: 3 classes, default 2vcpu-4gb
ok    class 1vcpu-2gb                  plugin /usr/bin/fleeting-plugin-aws works; group gitlab-runner-amd64-1vcpu-2gb-asg has 0 instances
ok    class 2vcpu-4gb                  plugin /usr/bin/fleeting-plugin-aws works; group gitlab-runner-amd64-asg has 0 instances
ok    class 8vcpu-16gb                 plugin /usr/bin/fleeting-plugin-aws works; group gitlab-runner-amd64-8vcpu-16gb-asg has 0 instances
ok    gitlab ci                        public template: 3 of its classes are here (1vcpu-2gb, 2vcpu-4gb, 8vcpu-16gb)
ok    github owner/api                 token can list jobs and runners
ok    config.toml                      /etc/gitlab-runner/config.toml: fleetci's [[runners]] entry is complete
```

A `FAIL` line says what is wrong. Run it again by hand with
`AWS_DEFAULT_REGION=<region> fleetci check -config /etc/fleetci/amd64.toml -runner-config /etc/gitlab-runner/config.toml`.
`config.toml: no [[runners]] entry` on a pool means its GitLab token was
invalid at boot, so it was left out (`on_invalid_token`); a main runner's
invalid token stops the Manager's boot instead.

Start one job on each forge:

```
vm acquired       vm=fleetci-8vcpu-16gb-… class=8vcpu-16gb      (the GitLab job)
runner starting   repo=owner/api queued_job=build class=8vcpu-16gb vm=… waited=1s
```

## Operating it

| What | How |
| --- | --- |
| A GitLab job fails waiting for a VM | its class was full for `acquire_timeout`. Raise the class's `max_instances` (and the account's spot quota), or its `acquire_timeout`. |
| `fleetci check` fails on `concurrent` | the runner's classes hold more jobs than gitlab-runner's `concurrent`. The module adds the extra classes' share to `runner_manager.maximum_concurrent_jobs`; raise that for the runner's own class. |
| A GitHub job never starts | the gitlab-runner log says why: an unknown class, an extra label, or no VM. It stays queued and is tried again. |
| A spot VM is reclaimed mid-job | the GitLab job fails and can be retried. A GitHub job stays "in progress" for about 4 minutes, then fails; re-run it. Once the notice is out, no new job goes to that VM (the log says "the cloud is taking this vm back"). |
| fleetci's plugin dies | `fleetci-watchdog@amd64` restarts gitlab-runner after 30 s, which also ends the other runners' jobs. |
| The GitHub token expires | put a new one in the SSM parameter and replace the Manager (`tofu apply -replace=module.runners.aws_instance.runner_manager`): the token is read at boot. |
| Add a class or a repo | change the runner's fleetci block and apply; the Manager is replaced. |
| Upgrade fleetci | raise `fleetci_version` and apply. |
| Turn fleetci off | remove the fleetci block: the runner goes back to fleeting-plugin-aws on its own group, and the extra groups are destroyed. Keeping the fleetci image works with `username = "ubuntu"` in its connector options. |
