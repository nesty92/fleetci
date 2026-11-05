# M1: real microVMs next to containers, on one machine

M0 with real VMs. `4vcpu-6gb` runs Cloud Hypervisor microVMs through
[fleeting-plugin-fleetingd](https://github.com/helmholtzcloud/fleeting-plugin-fleetingd);
`1vcpu-2gb` stays on `fleeting-plugin-dockervm`, or becomes a second
fleetingd group. GitLab jobs and GitHub Actions jobs pick either one, as in
M0.

```
fleetci-m1 container (/dev/kvm, /dev/net/tun, NET_ADMIN, own netns)
  gitlab-runner ─ fleeting-plugin-fleetci
    class 1vcpu-2gb ─ dockervm ─ machine's Docker socket ─► container "VM"
    class 4vcpu-6gb ─ fleetingd ─ cloud-hypervisor ─ tap + nftables SNAT ─► microVM
```

fleetingd's tap devices and nftables tables live in the container's network
namespace, so the machine's network and firewall are left alone.

When our fork, [nesty92/fleeting-plugin-fleetingd](https://github.com/nesty92/fleeting-plugin-fleetingd),
is checked out next to fleetci, the kit builds and uses it. Its VMs are
restored from a snapshot rather than booted: ready in under a second.

## Run

```sh
FLEETCI_GITHUB_REPOS=owner/repo hack/m1/up.sh   # GitHub part is optional
docker logs -f fleetci-m1
hack/m1/down.sh
```

| Variable | Default | |
| --- | --- | --- |
| `FLEETCI_M1_SMALL` | `dockervm` | `fleetingd` makes `1vcpu-2gb` a second fleetingd group |
| `FLEETCI_M1_SNAPSHOT` | `true` | `false` boots every VM (fork only) |
| `FLEETCI_FLEETINGD_SRC` | `../fleeting-plugin-fleetingd` | the fork's checkout |
| `FLEETCI_GITHUB_CACHE_URL` | | the Actions cache server from `hack/cache/up.sh` |

It needs `/dev/kvm` and reuses M0's runner token
(`~/.config/fleetci/m0-runner.token`), so don't run M0 and M1 at once.
`snap/` holds the snapshot and Firecracker experiments.
Measurements are in [docs/measurements.md](../../docs/measurements.md).

## Limits

- Upstream fleetingd allows one class per machine and wants a runner
  restart every 24 h for a fresh image; the fork lifts both.
- Files under `~/.local/share/fleetci-m1` belong to root (written from
  the container). Remove them with
  `docker run --rm -v ~/.local/share/fleetci-m1:/d ubuntu rm -rf /d/vms /d/images`.
