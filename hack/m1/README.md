# M1: real microVMs next to containers, on one machine

M0 with one class swapped for real VMs. `4vcpu-6gb` boots Cloud Hypervisor
microVMs through
[fleeting-plugin-fleetingd](https://github.com/helmholtzcloud/fleeting-plugin-fleetingd);
`1vcpu-2gb` stays on `fleeting-plugin-dockervm`. GitLab jobs and GitHub
Actions jobs pick either one, as in M0.

```
fleetci-m1 container (/dev/kvm, /dev/net/tun, NET_ADMIN, own netns)
  gitlab-runner ─ fleeting-plugin-fleetci
    class 1vcpu-2gb ─ dockervm ─ machine's Docker socket ─► container "VM"
    class 4vcpu-6gb ─ fleetingd ─ cloud-hypervisor ─ tap + nftables SNAT ─► microVM
```

fleetingd's tap devices and nftables tables live in the container's network
namespace, so the machine's network and firewall are left alone.

## Run

```sh
FLEETCI_GITHUB_REPOS=owner/repo hack/m1/up.sh   # GitHub part is optional
docker logs -f fleetci-m1
hack/m1/down.sh
```

It needs `/dev/kvm` and reuses M0's runner token
(`~/.config/fleetci/m0-runner.token`), so don't run M0 and M1 at once.

## What fleetingd does

On the first job it downloads the Ubuntu 26.04 cloud image and kernel. It
boots a prebuild VM that runs `vm_prebuild_cloudinit_extra_cmds`, which
installs Docker and actions/runner, then cleans cloud-init and shuts down.
Every job VM is a copy-on-write clone of that image with a fresh cloud-init
seed. A VM runs one job and is then removed.

Measured on a 16-thread laptop (2026-11-04):

| Step | Time |
| --- | --- |
| Image download, resize and prebuild (once) | 2 min 57 s |
| microVM start to ready (cloud-init, SSH, Docker) | 16.1–22.8 s, 6 VMs |
| GitHub JIT runner: VM ready to job running | about 7 s |

## Limits

- fleetingd allows one class per machine: its nftables table names and
  group ID are fixed, and `vm_subnet` must be `172.16.120.`.
- The image follows Ubuntu's daily build and is rebuilt when that changes.
  fleetingd's README suggests restarting the runner every 24 h.
- Files under `~/.local/share/fleetci-m1/vms` belong to root (written from
  the container). Remove them with
  `docker run --rm -v ~/.local/share/fleetci-m1:/d ubuntu rm -rf /d/vms`.
