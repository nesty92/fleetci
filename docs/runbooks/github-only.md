# GitHub only

`fleetci serve` runs GitHub Actions jobs on fleetci's classes, with no
GitLab and no gitlab-runner. It lists the repos' queued jobs, gets a VM of
each job's class, registers a one-job runner on it and removes the VM when
the job ends.

```
fleetci serve (systemd)
  classes: 2vcpu-4gb, 4vcpu-8gb, …   (any fleeting plugin: aws, fleetingd, googlecloud, azure)
  [github] repos → queued jobs → VM → just-in-time runner → job → VM removed
```

Use it for GitHub-only installs. Beside a gitlab-runner, it also keeps GitHub
jobs going while gitlab-runner restarts; then its classes need groups of
their own.

## 1. Install

`fleetci` and each class's fleeting plugin; no gitlab-runner, no
`fleeting-plugin-fleetci`:

```sh
v=0.6.0 arch=linux-amd64 # x-release-please-version
base=https://github.com/nesty92/fleetci/releases/download/v$v
curl -fsSLO "$base/fleetci-$arch" -fsSLO "$base/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
install -m 755 "fleetci-$arch" /usr/local/bin/fleetci
```

The plugins and their groups are as in the [README](../../README.md#install)
for a cloud, or [Bare metal with cloud spikes](bare-metal-with-cloud-spikes.md)
for your own server, `overflow` included.

## 2. The token

A fine-grained token for the repos with *Actions: Read* and
*Administration: Read and write*, in `/etc/fleetci/github.token`, mode 0600.

## 3. github.toml

```toml
# /etc/fleetci/github.toml
default_class = "2vcpu-4gb"

[classes.2vcpu-4gb]
plugin = "fleeting-plugin-aws"
max_instances = 10
plugin_config = { name = "fleetci-gh-2vcpu-4gb", region = "eu-west-3" }
connector_config = { username = "ubuntu", use_external_addr = true }

[classes.4vcpu-8gb]
plugin = "fleeting-plugin-aws"
max_instances = 4
plugin_config = { name = "fleetci-gh-4vcpu-8gb", region = "eu-west-3" }
connector_config = { username = "ubuntu", use_external_addr = true }

[github]
repos = ["owner/api", "owner/web"]
token_file = "/etc/fleetci/github.token"
```

The other `[github]` keys are in [GitLab and GitHub](gitlab-and-github.md#3-fleetcitoml).
When it runs beside a gitlab-runner with fleetci, this file's groups must
be different ones (other Auto Scaling groups, other fleetingd `name`s), and
only this file has `[github]`.

```sh
fleetci check -config /etc/fleetci/github.toml -runner-config ""
```

## 4. The service

```ini
# /etc/systemd/system/fleetci-serve.service
[Unit]
Description=fleetci: GitHub Actions jobs on fleetci's classes
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/fleetci serve -config /etc/fleetci/github.toml
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload && systemctl enable --now fleetci-serve
journalctl -u fleetci-serve -f
```

When `serve` stops, it removes its VMs. No watchdog is needed: systemd
restarts it, and a class's plugin that dies is started again by fleetci.

## 5. Check it

Run a workflow with `runs-on: fleetci-2vcpu-4gb`:

```
fleetci serving      classes=2 default_class=2vcpu-4gb
serving github jobs  repos=[owner/api owner/web]
runner starting      repo=owner/api queued_job=build class=2vcpu-4gb vm=fleetci-2vcpu-4gb-… waited=38s
runner took a job    …
runner done          …
```

## Operating it

| What | How |
| --- | --- |
| A job never starts | the log says why: an unknown class, an extra label, or no VM yet. It stays queued and is tried again. |
| `serve` crashes | systemd starts it within 2 s; jobs on its VMs fail, queued ones are picked up again. |
| A VM is lost mid-job | GitHub shows the job "in progress" for about 4 minutes, then fails it; re-run it. |
| Token expires | `fleetci check` fails on the GitHub line and the log shows 401s. Replace the file and restart: the token is read at start. |
| Upgrade | replace the binary, `fleetci check`, `systemctl restart fleetci-serve`. Running jobs fail, so do it while quiet. |
| Two `serve`s on the same repos | don't: each starts a runner for the same queued job, and one VM is wasted. One `serve` per set of repos. |
