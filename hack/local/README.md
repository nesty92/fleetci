# Local kit: fleetci on one machine

Stock gitlab-runner, fleetci's plugin and, optionally, the GitHub side, in
one container, `fleetci-local`. Two classes, `1vcpu-2gb` and `4vcpu-8gb`;
GitLab jobs and GitHub Actions jobs pick either one.

```
fleetci-local container
  gitlab-runner ─ fleeting-plugin-fleetci        (and fleetci serve, optionally)
    class 1vcpu-2gb ─┐
    class 4vcpu-8gb ─┴─ microvm:   fleetingd ─ cloud-hypervisor ─ tap + nftables SNAT ─► microVM
                        container: dockervm ─ machine's Docker socket ─► privileged container
```

The VMs are of one of two kinds:

- **microVMs**: Cloud Hypervisor through our fork of fleetingd,
  [nesty92/fleeting-plugin-fleetingd](https://github.com/nesty92/fleeting-plugin-fleetingd),
  from its release, or built from its checkout next to fleetci when there
  is one. Needs `/dev/kvm`. VMs are restored from a snapshot rather than
  booted, so they're ready in under a second. The taps and nftables tables
  live in the container's network namespace, so the machine's network and
  firewall are left alone.
- **containers**: `fleeting-plugin-dockervm`, privileged containers running
  Ubuntu 24.04 with their own Docker, like GitHub's `ubuntu-latest`. Any
  machine with Docker. Like EC2 Instance Connect, the plugin hands out a new key
  on every `ConnectInfo`, which the VM accepts for two minutes.

## Run

```sh
FLEETCI_GITHUB_REPOS=owner/repo hack/local/up.sh   # GitHub part is optional
docker logs -f fleetci-local
hack/local/down.sh
```

| Variable | Default | |
| --- | --- | --- |
| `FLEETCI_LOCAL_VMS` | `microvm` with `/dev/kvm`, else `container` | `mixed`: 1vcpu-2gb on containers, 4vcpu-8gb on microVMs |
| `FLEETCI_LOCAL_SNAPSHOT` | `true` | `false` boots every microVM |
| `FLEETCI_LOCAL_OVERFLOW` | `false` | `true` adds `4vcpu-8gb-cloud`, 3 containers standing in for a cloud, as 4vcpu-8gb's `overflow` |
| `FLEETCI_LOCAL_GITHUB` | `plugin` | `serve` runs GitHub jobs in a `fleetci serve` of their own, on groups `gh1` and `gh4` |
| `FLEETCI_FLEETINGD_SRC` | `../fleeting-plugin-fleetingd` | the fork's checkout |
| `FLEETCI_GITHUB_CACHE_URL` | | the Actions cache server from `hack/cache/up.sh` |

The runner token is read from `~/.config/fleetci/runner.token` and written
only into `~/.local/share/fleetci-local/config.toml` (mode 0600). The GitHub
token is in `~/.config/fleetci/github.token`. Both classes are on fleetci's
public template, so GitLab jobs pick them with it alone; see
[Picking a class on GitLab](../../README.md#picking-a-class-on-gitlab).

Measurements are in [docs/measurements.md](../../docs/measurements.md).

## What it showed about gitlab-runner

- `vm_isolation.image` is expanded with the job's variables, so
  `class-${FLEETCI_CLASS}` reaches fleetci as `class-4vcpu-8gb`.
- `vm_isolation.connector_config.key_path` is ignored. The runner logs in
  to a VM with `password` only. fleetci ends that connection itself, so the
  password is a placeholder.
- gitlab-runner 19.5 needs an explicit `timeout` on both connectors.
- The runner checks the instance by running `exit 0`, and reaches Docker
  with `docker system dial-stdio`, so the proxy forwards command sessions
  as well as socket streams.
- gitlab-runner never starts its fleeting plugin again once the plugin dies, so
  the kit runs `fleetci watchdog` beside it.

## Limits

- Files under `~/.local/share/fleetci-local` belong to root, because the
  container writes them. Remove them with
  `docker run --rm -v ~/.local/share/fleetci-local:/d ubuntu sh -c 'rm -rf /d/vms-* /d/images'`.

## Docker integration test

```sh
docker build -t fleetci-local-vm hack/local/vm
go test -tags docker ./internal/fleet/
```
