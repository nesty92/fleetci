# AWS with the terraform module

[terraform-aws-gitlab-runner-autoscaler](https://github.com/nesty92/terraform-aws-gitlab-runner-autoscaler)
runs gitlab-runner on a Runner Manager instance with runner pools, each an
Auto Scaling group. fleetci on a pool changes one thing: the pool's runner
gets its VMs through `fleeting-plugin-fleetci` instead of
`fleeting-plugin-aws`. The pool keeps its runner, token, tags, docker
options and cache. Its group becomes one class, the default, and each extra
class gets a group of its own.

```
Runner Manager
  gitlab-runner
    runner-amd64   fleeting-plugin-aws        → gitlab-runner-amd64-asg    (unchanged)
    runner-ci      fleeting-plugin-fleetci-ci → gitlab-runner-ci-asg        class 2vcpu-4gb (default)
                                              → gitlab-runner-ci-1vcpu-2gb-asg
                                              → gitlab-runner-ci-8vcpu-16gb-asg
                   [github] owner/api, owner/web → the same classes
```

The module writes `/etc/fleetci/ci.toml`, links the plugin as
`/usr/bin/fleeting-plugin-fleetci-ci`, runs `fleetci check` and starts
`fleetci-watchdog@ci`. Nothing is installed by hand.

## 1. The image

A pool with fleetci, and each of its classes, boots a fleetci image: Ubuntu
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

- **GitLab:** register a runner for the pool and give it the tag `fleetci`.
  Keep "Run untagged jobs" as you had it: untagged jobs go to the default
  class. Store its token in SSM, as for any pool.
- **GitHub** (optional): a fine-grained token for the repos with
  *Actions: Read* and *Administration: Read and write*, in an SSM
  SecureString.

## 3. The pool

```hcl
module "runners" {
  source = "github.com/nesty92/terraform-aws-gitlab-runner-autoscaler?ref=v3.9.0"
  # … VPC, subnets, the Manager, the per-architecture runners

  runner_pools = {
    ci = {
      architecture = "amd64"
      ami_id       = var.fleetci_ami_amd64
      token        = { name = "gitlab-runner-ci-token", arn = aws_ssm_parameter.ci.arn, region = "eu-north-1" }
      instance     = { type = "c7a.large", instance_type_overrides = [{ instance_type = "c7i.large" }] }
      autoscaler   = { max_instances = 4, capacity_per_instance = 1, max_use_count = 1 }

      fleetci = {
        class = "2vcpu-4gb"            # the pool's own group
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
  }
}
```

| Key | Default | |
| --- | --- | --- |
| `class` | | the pool's own class; its size, reuse and warm VMs come from the pool's `instance`, `autoscaler` and `policy` |
| `acquire_timeout` | `10m` | how long a GitLab job waits for a VM of the pool's class |
| `classes.<name>` | | `instance_types` (several make a mixed instances policy), `ami_id`, `architecture` (`amd64`), `max_instances` (4), `capacity_per_instance` and `max_use_count` (1), `policy` (none: every VM boots for its job), `user_data`, `username` (`ubuntu`), `volume_size` (30), `subnet_ids`, `private_address_only` (true), `spot_allocation_strategy`, `acquire_timeout` |
| `github` | | `repos` and the SSM parameter of the GitHub token |
| `github_runner_version` | `2.338.2` | keep it equal to the image's `runner_version`, or each GitHub job downloads the runner |
| `watchdog` | `true` | restarts gitlab-runner when the pool's plugin has been gone for 30 s |

`fleetci_version` (a module input) picks the fleetci release. Class names
must match `^[a-z0-9][a-z0-9-]{0,29}$`. Prefer non-burstable types: a T type
bills surplus CPU credits for a CPU-bound job.

`tofu apply` replaces the Manager, as any pool change does.

## 4. Reusing VMs

A class may keep a VM for several jobs, to keep pulled images and caches:
`capacity_per_instance` and `max_use_count` on an extra class, the pool's
`autoscaler` for its own class. docker-autoscaler's default
`max_use_count` is 10, so a pool reuses VMs unless it says otherwise.

That is for GitLab only. GitHub steps run on the VM itself, so a job could
read or change another job's VM, and the plan stops when a pool with
`github` has a class that reuses VMs. To reuse VMs for GitLab and serve
GitHub too, use two pools:

```hcl
runner_pools = {
  gitlab = {                           # reuses VMs: max_use_count 10
    architecture = "amd64"
    ami_id       = var.fleetci_ami_amd64
    token        = { … }
    fleetci      = { class = "2vcpu-4gb" }
  }
  github = {                           # one job per VM, serves GitHub
    architecture = "amd64"
    ami_id       = var.fleetci_ami_amd64
    token        = { … }               # a runner token of its own
    autoscaler   = { max_use_count = 1 }
    fleetci = {
      class  = "2vcpu-4gb"
      github = { repos = ["owner/api"], token = { … } }
    }
  }
}
```

Each pool runs its own fleetci with its own groups. A GitHub repo can be
served by one pool only. Single-use classes are fine for both forges on one
pool: every VM is removed after its job.

## 5. The workflows

<!-- x-release-please-start-version -->
```yaml
# GitLab
include:
  - remote: https://raw.githubusercontent.com/nesty92/fleetci/v0.4.0/templates/gitlab-ci.yml

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
each also `-arm`). For other class names, commit the pool's `ci_include`:

```sh
tofu output -json fleetci | jq -r .ci.ci_include > .gitlab/fleetci.yml
```

`tofu output fleetci` also lists each class's Auto Scaling group and GitHub
label.

## 6. Check it

On the Manager (SSH, or Session Manager with `runner_manager.enable_ssm_agent = true`):

```sh
grep -A20 'fleetci check' /var/log/cloud-init-output.log   # every class, GitHub access, config.toml
systemctl status gitlab-runner fleetci-watchdog@ci
journalctl -u gitlab-runner -f
```

Start one job on each forge:

```
vm acquired       vm=fleetci-8vcpu-16gb-… class=8vcpu-16gb      (the GitLab job)
runner starting   repo=owner/api queued_job=build class=8vcpu-16gb vm=… waited=1s
```

## Operating it

| What | How |
| --- | --- |
| A GitLab job fails waiting for a VM | its class was full for `acquire_timeout`. Raise the class's `max_instances` (and the account's spot quota), or its `acquire_timeout`. |
| A GitHub job never starts | the gitlab-runner log says why: an unknown class, an extra label, or no VM. It stays queued and is tried again. |
| A spot VM is reclaimed mid-job | the GitLab job fails and can be retried. A GitHub job stays "in progress" for about 4 minutes, then fails; re-run it. |
| fleetci's plugin dies | `fleetci-watchdog@ci` restarts gitlab-runner after 30 s, which also ends the other runners' jobs. |
| The GitHub token expires | put a new one in the SSM parameter and replace the Manager (`tofu apply -replace=module.runners.aws_instance.runner_manager`): the token is read at boot. |
| Add a class or a repo | change the pool's `fleetci` and apply; the Manager is replaced. |
| Upgrade fleetci | raise `fleetci_version` and apply. |
| Turn fleetci off | remove `fleetci` from the pool: it goes back to fleeting-plugin-aws on its own group, and the extra groups are destroyed. Keeping the fleetci image works if `connector.username = "ubuntu"`. |
