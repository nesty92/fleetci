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
| microVM start to ready (cloud-init, SSH, Docker), stock image | 16.1–22.8 s, 6 VMs |
| same, image without snapd & co. and with `chrony-wait` masked | 9.3–15.1 s, 4 VMs |
| fleetingd's copy of the 5.4 GB image before each start | 1.9–3.0 s |
| GitHub JIT runner: VM ready to job running | about 7 s |

What is left of a cold boot is fleetingd's own per-VM user-data: `apt update`
and `apt upgrade` on every boot, 3 s, or 12 s when Ubuntu has published
updates. The same image booted without them reaches Docker in 6.7 s
(kernel 0.2 s, userspace 5.4 s). `chrony-wait.service` alone held boot up
for 6 s: `multi-user.target`, and so the end of cloud-init, waits for the
clock to sync.

## Snapshots

`snap/run.sh [copyonwrite|ondemand|copy] [clones]` boots one VM from
fleetingd's image, snapshots it once Docker answers, and restores clones
side by side. Each clone runs in its own network namespace with the
template's IP and MAC, so its network needs no change. Its disk is a qcow2
overlay on the frozen template disk. After the restore it gets a stepped
clock, new SSH host keys and a random machine id.

3 clones at once, 4 vCPU and 6 GB each:

| Restore mode | Restore to `docker version` over SSH | Memory per clone |
| --- | --- | --- |
| `copyonwrite` | 0.34–0.37 s | 620–650 MB resident, 91–101 MB private |
| `ondemand` (userfaultfd) | 0.80–0.88 s | 6.1 GB, all private |

Identity fixes take another 0.35–0.48 s, and `docker run` works in each clone.
Writing the snapshot takes 2.3 s and 6.1 GB of disk: the guest's whole memory.
With `copyonwrite` the clones share the snapshot's pages through the page
cache, so three cost about one copy of what they read plus 100 MB each.
`ondemand` faults in all 6 GB per clone. No fleeting plugin does this yet.

### Firecracker, for comparison

`snap/fc-run.sh` runs the same experiment on Firecracker 1.17 with its CI
kernel (6.1) and the same image, sizes and clones. The CI kernel has no GPT,
ISO9660/FAT or nf_tables. So the script extracts the root partition to a plain
ext4 file (once, 53 s) and writes the cloud-init seed into it. It also drops
the EFI mount from fstab, and the guest switches to iptables-legacy.

| | Cloud Hypervisor | Firecracker |
| --- | --- | --- |
| cold boot to `docker version` | 6.7 s (kernel 0.2 s) | 6.6 s (kernel 0.8 s) |
| restore to `docker version` | 0.34–0.37 s | 0.62 s |
| memory per clone, resident / private | 620 / 93 MB | 340 / 84 MB |
| clone disk | qcow2 overlay, instant | raw only: 4.4 GB copy, 4.6–5.3 s |
| RNG after restore | reseed by hand | VMGenID reseeds it |
| `docker run` on the default bridge | works | fails: the CI kernel lacks the iptables raw table |

Firecracker uses less memory, but it needs a kernel of our own for Docker's
networking. Without reflinks or device-mapper snapshots, every clone copies
its disk. Cloud Hypervisor runs Ubuntu's own kernel and image as they are.

## Limits

- fleetingd allows one class per machine: its nftables table names and
  group ID are fixed, and `vm_subnet` must be `172.16.120.`.
- The image follows Ubuntu's daily build and is rebuilt when that changes.
  fleetingd's README suggests restarting the runner every 24 h.
- Files under `~/.local/share/fleetci-m1/vms` belong to root (written from
  the container). Remove them with
  `docker run --rm -v ~/.local/share/fleetci-m1:/d ubuntu rm -rf /d/vms`.
