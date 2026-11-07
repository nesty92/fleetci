# GitLab and GitHub from one runner

One gitlab-runner serves GitLab jobs and GitHub Actions jobs on the same
classes. fleetci's plugin inside gitlab-runner does both; no other process
runs. GitLab jobs pick a class with `extends: .fleetci-<class>`, GitHub
jobs with `runs-on: fleetci-<class>`, and both share each class's VMs and
limits.

```
gitlab-runner ─ fleeting-plugin-fleetci
  GitLab:  gitlab-runner takes a job, asks fleetci for a VM of its class
  GitHub:  fleetci lists the repos' queued jobs, gets a VM, starts a one-job runner on it
  classes: 1vcpu-2gb, 4vcpu-8gb, …  (any fleeting plugin: aws, fleetingd, …)
```

Use this when one machine serves both forges and you want one thing to run.
Choose [GitHub only](github-only.md) instead, or `fleetci serve` beside
gitlab-runner, when GitHub jobs must keep running while gitlab-runner
restarts.

## 1. Install

Install gitlab-runner, fleetci, its plugin and each class's fleeting plugin
as the [README](../../README.md#install) describes, or as
[Bare metal with cloud spikes](bare-metal-with-cloud-spikes.md) does on your
own server. On AWS, `github` in the fleetci block of a terraform module runner
does all of this ([AWS with the terraform module](aws-with-the-module.md)).

## 2. Tokens

- **GitLab:** a runner with the tag `fleetci` and "Run untagged jobs" off;
  its token in `/etc/fleetci/runner.token`.
- **GitHub:** a fine-grained token for the repos with *Actions: Read* and
  *Administration: Read and write* (it registers one runner per job), in
  `/etc/fleetci/github.token`, mode 0600.

## 3. fleetci.toml

Add a `[github]` table to the file gitlab-runner's plugin reads:

```toml
default_class = "2vcpu-4gb"

[classes.2vcpu-4gb]
# …

[classes.4vcpu-8gb]
# …

[github]
repos = ["owner/api", "owner/web"]
token_file = "/etc/fleetci/github.token"
# cache_url = "http://10.0.1.5:3000/"   # optional: Actions cache on your storage
```

| Key | Default | |
| --- | --- | --- |
| `repos` | | `owner/name`, each repo whose jobs fleetci serves |
| `orgs` | | organizations whose every repo fleetci serves, with runners on the org |
| `token_file` | | the GitHub token |
| `app_id`, `app_private_key_file` | | a GitHub App to sign in as, in place of `token_file` |
| `webhook.listen`, `webhook.secret_file` | | take `workflow_job` deliveries, see the README |
| `webhook.path` | `/` | the URL path deliveries arrive at |
| `webhook.tls_cert_file`, `webhook.tls_key_file` | | serve the webhook over HTTPS |
| `label_prefix` | `fleetci` | `runs-on: fleetci` is the default class, `fleetci-<class>` a class |
| `poll_interval` | `10s`, `5m` with a webhook | how often queued jobs are listed |
| `idle_timeout` | `10m` | how long a runner may wait for a job (say its job was cancelled while the VM booted) before its VM is given back |
| `runner_version` | `2.338.2` | the actions/runner release to run; images with it in `~/actions-runner-<version>` skip the download |
| `runner_group_id` | `1` | the runner group to register in; for `orgs`, the group must allow the repos |
| `cache_url` | | an Actions cache server, see the README |
| `api_url` | `https://api.github.com` | GitHub Enterprise Server's API |

Then:

```sh
fleetci runner-config -global -token-file /etc/fleetci/runner.token > /etc/gitlab-runner/config.toml
fleetci check            # lists "github repo owner/api: can list jobs and runners"
systemctl restart gitlab-runner
```

`fleetci runner-config` raises `concurrent` by the classes' limits. GitHub
jobs don't count against it: fleetci starts their runners itself, within
each class's `max_instances`.

Every class must run one job per VM (`capacity_per_instance` and
`max_use_count` 1, the defaults): GitHub steps run on the VM itself, so a
job could read or change another job's VM, and `fleetci check` fails on a
class that reuses VMs. To reuse VMs for GitLab, keep `[github]` out of that
file and serve GitHub from another (a second plugin config, or
`fleetci serve`) with single-use classes and groups of their own.

Run fleetci's watchdog beside gitlab-runner, as in
[Bare metal with cloud spikes](bare-metal-with-cloud-spikes.md#4-gitlab-runner-and-the-watchdog):
both forges stop while fleetci's plugin is gone.

## 4. The workflows

```yaml
# GitHub
jobs:
  build:
    runs-on: fleetci-4vcpu-8gb     # or fleetci for the default class
```

<!-- x-release-please-start-version -->
```yaml
# GitLab
include:
  - remote: https://raw.githubusercontent.com/nesty92/fleetci/v0.6.0/templates/gitlab-ci.yml
build:
  extends: .fleetci-4vcpu-8gb
```
<!-- x-release-please-end -->

A GitHub job asking for a class fleetci lacks, or with other labels than
`self-hosted` and `linux`, stays queued and is logged once.

## 5. Check it

Start one job on each forge and watch gitlab-runner's log:

```
runner starting   repo=owner/api queued_job=build class=4vcpu-8gb vm=fleetci-4vcpu-8gb-… waited=1s
vm acquired       vm=fleetci-4vcpu-8gb-… class=4vcpu-8gb      (the GitLab job)
```

## Operating it

| What | How |
| --- | --- |
| A GitHub job never starts | the log says why: an unknown class, an extra label, or no VM ("no vm for job; retrying on the next poll"). It stays queued and is tried again. |
| GitHub rate-limits the token | the log says "rate limited by github until …". fleetci sends nothing until then, as GitHub asks, and starts no VM; jobs wait. A token shared with other tools spends the same limit. |
| Runners left registered | when GitHub fails to answer a runner's registration or removal, fleetci looks the runner up by its name (the VM's) and removes it on a later poll ("removed an orphaned runner"). It keeps trying for a day. |
| A VM is lost during a GitHub job | the job stays "in progress" on GitHub for about 4 minutes, then fails. GitHub never queues it again; re-run it. |
| gitlab-runner restarts | GitHub jobs running on its VMs fail with it, like GitLab's. To keep GitHub apart, move `[github]` to a `fleetci serve` with classes of its own ([GitHub only](github-only.md)). |
| Token expires | `fleetci check` fails on the GitHub line, and the log shows 401s. Replace the file and restart gitlab-runner: the token is read at start. |
| Add a repo | add it to `repos` and restart gitlab-runner. |
