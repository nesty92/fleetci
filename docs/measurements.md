# Measurements

Local runs of the M1 kit (`hack/m1`) on a 16-thread laptop, November 2026.
Cloud Hypervisor v54, Firecracker 1.17, Ubuntu 26.04 guests.

## VM start, fleetingd

From the VM's start to SSH and `docker version` answering.

| Setup | Time |
| --- | --- |
| upstream fleetingd, stock image | 16.1–22.8 s |
| same, without snapd & co. and with `chrony-wait` masked | 9.3–15.1 s |
| fork: qcow2 overlay, no per-boot apt, 4 vCPU | 5.8–7.3 s |
| fork, 1 vCPU | 9.3–10.2 s |
| fork, restored from a snapshot | 0.29–0.58 s |

Once per host: image download and prebuild, 3 min; a second group reuses
the image in 1 s. Once per VM size: snapshot build, 11–13 s. The
snapshot's memory file is sparse, about 800 MB for 6 GB of guest RAM.

What slowed cold boots:
- upstream's 5.4 GB image copy per VM, 1.9–3.0 s;
- `apt update` and `upgrade` on every boot, 3–12 s;
- `chrony-wait`, 6 s;
- `systemd-networkd-wait-online`, 1.1–2.0 s.

## Snapshot restore modes

`hack/m1/snap/run.sh`, 3 clones at once, 4 vCPU and 6 GB each.

| Mode | Restore to `docker version` | Memory per clone |
| --- | --- | --- |
| `copyonwrite` | 0.34–0.37 s | 620–650 MB resident, 91–101 MB private |
| `ondemand` (userfaultfd) | 0.80–0.88 s | 6.1 GB, all private |

## Cloud Hypervisor and Firecracker

`hack/m1/snap/fc-run.sh` and `boot-run.sh`, same image and sizes.
Firecracker uses Ubuntu's kernel, `quiet`, with disks on btrfs reflinks.

| | Cloud Hypervisor | Firecracker |
| --- | --- | --- |
| cold boot to `docker version` | 5.2–6.3 s | 5.1–5.2 s |
| restore to `docker version` | 0.34–0.37 s | 0.32 s |
| memory per clone, resident / private | 620 / 93 MB | 420 / 82 MB |
| clone disk | qcow2 overlay | raw only: reflink or a full copy (2.3–5.3 s) |
| RNG after restore | reseeded by the plugin | VMGenID |

Firecracker's serial console made its boot and restore slow (1.54 s); quiet,
the two are alike. Its CI kernel lacks the iptables raw table, so Docker's
bridge fails there. Cloud Hypervisor won: Ubuntu's own kernel and image,
qcow2 overlays.

## Real-sized jobs

The playground's `heavy` workflow (GitHub) and `HEAVY=1` pipeline (GitLab),
on restored VMs, with more 4vcpu-6gb jobs than the class allows at once (2).

| Job | GitHub |
| --- | --- |
| valkey 8.1.9: build, list/hash/expire test suites | 1 min 35 s |
| prometheus v3.5.5: `go build ./cmd/prometheus` | 1 min 59 s |
| caddy v2.10.2: multi-stage `docker buildx` build, run, curl | 1 min 23 s |
| postgres 17 service, `pgbench -s 50`, 8 clients for 30 s | 53 s |
| 4 GB held in memory, 8 GB written and checked, 20 containers | 1 min 0 s |
| cobra's tests, 1vcpu-2gb | 1 min 19 s |

The GitLab pipeline passed the same jobs except the Docker build, which an
unprivileged GitLab job can't run.

## A plugin that dies

`kill -9` on the 4vcpu-6gb plugin while both its VMs ran jobs:

| | Before | Now |
| --- | --- | --- |
| class plugin running again | never, until fleetci restarted | same second |
| the two jobs on its VMs fail | after 2 min 18 s (TCP gave up) | after 28 s (keepalive) |
| new VMs for the queue | one at a time: reused IDs clashed | two, 31 s after the kill |

## Chaos rounds

On M1, with real jobs running on both forges.

| Fault | What happened |
| --- | --- |
| a VM's hypervisor killed | its job failed after 19 s; the VM was replaced |
| a VM frozen (`SIGSTOP`) | its job failed after 29 s; the frozen VM was killed and replaced |
| the 4vcpu-6gb plugin killed | running again the same second; its two jobs failed after 28 s |
| the fleetci plugin killed | its class plugins and VMs died with it; gitlab-runner never starts it again, so `fleetci watchdog` restarted gitlab-runner after 40 s |
| no internet for 60 s | job listing failed every 30 s; the running job paused and passed; nothing else failed |
| a job fills the disk | that job failed (`docker run`: exit 125); its runner still reported it |
| a job asks for 8 GB on 6 GB | `MemoryError`; the VM and runner stayed up |
| a job powers its VM off | dropped after about 30 s; GitHub reports "lost communication" |
| the whole M1 container restarted | new VMs 6 s later; no taps, files or processes left over |

A job whose VM is lost stays "in progress" on GitHub until GitHub gives up
on its runner, about 4 minutes later; GitHub never queues it again.
