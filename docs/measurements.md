# Measurements

Runs of the local kit ([hack/local](../hack/local)) on a 16-thread laptop,
November 2026.
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

3 clones at once, 4 vCPU and 6 GB each.

| Mode | Restore to `docker version` | Memory per clone |
| --- | --- | --- |
| `copyonwrite` | 0.34–0.37 s | 620–650 MB resident, 91–101 MB private |
| `ondemand` (userfaultfd) | 0.80–0.88 s | 6.1 GB, all private |

## Cloud Hypervisor and Firecracker

Same image and sizes.
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

With real jobs running on both forges.

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
| the kit's container restarted | new VMs 6 s later; no taps, files or processes left over |

With GitHub on `fleetci serve` (`FLEETCI_LOCAL_GITHUB=serve`) and GitLab on
gitlab-runner's plugin, each on its own groups:

| Fault | What happened |
| --- | --- |
| `fleetci serve` killed, both forges busy | back in 2 s (its restart loop); its VMs died with it; GitLab's kept running |
| the gitlab-runner plugin killed, both busy | GitHub's run passed untouched; the GitLab job on the plugin failed; the watchdog had it back in 46 s |

A job whose VM is lost stays "in progress" on GitHub until GitHub gives up
on its runner, about 4 minutes later; GitHub never queues it again.

The scripts behind the restore-mode and Firecracker runs were removed once
done. They're in git history under `hack/m1/snap` (commit `3514838`).

## Embedded GitLab runner against the plugin

The same compatibility pipeline and GitHub workflow, started together, once
on gitlab-runner with fleetci's plugin and once on `fleetci serve` running
the same `config.toml` (`[gitlab] config_file`) with gitlab-runner v19.5.2
inside it. Classes: 1vcpu-2gb (3) and 4vcpu-8gb (2) on microVMs; the S3
cache on RustFS.

| Job | Plugin | Embedded |
| --- | --- | --- |
| default class, 4vcpu-8gb class, a service | passed | passed |
| artifacts, dotenv report, S3 cache between jobs | passed | passed |
| image entrypoint, `GIT_STRATEGY: none`, `parallel: 2`, `after_script` | passed | passed |
| `retry: 1` on exit 3 | failed twice, `script_failure` | the same |
| cancelled while running | `canceled`, VM removed | the same |
| a class the runner lacks | `runner_system_failure` after 10 s | `runner_configuration_error` after 1 s, no VM |
| GitHub's 4 jobs at the same time | passed | passed |

The embedded loop acquired and released 26 VMs and left none. gitlab-runner
registers its cache adapters and secret resolvers in its own main package;
until fleetci imported them, `[runners.cache]` was ignored ("No URL
provided").

## The small VM image

The image now called [github](../images/github.json), then
`images/small.json`, built locally with fleetingd's
`build-image` (Cloud Hypervisor 54.0, 2 vCPU, amd64), then used through
`vm_image_url` from an HTTP server on the same machine, for the playground
org's `basic` workflow (two jobs, 1vcpu-2gb).

| | |
| --- | --- |
| build, cloud image already downloaded | 3 min |
| image, compressed qcow2 | 2.17 GB with gitlab-runner's helper images, 1.45 GB without them and trimmed |
| image, decompressed on the host | 5.4 GB |
| download, check and decompress on start (local network) | 3.4 s |
| snapshot from the downloaded image | 14 s |
| dispatch to both jobs done | 35 s; the runners started 23 s after dispatch and ran 11 s, no runner downloaded |

## From a waiting job to its VM

The playground's `basic` workflow, two jobs at once on 1vcpu-2gb VMs
restored from a snapshot, from fleetci asking for the VMs to each job
holding one.

| Step | Before | After |
| --- | --- | --- |
| taskscaler decides to scale, then asks the plugin | 1.2 s | 0.46 s; up to 2 s, on taskscaler's 1 s tick and its provisioning loop |
| both VMs restored | 0.84 s, one after the other | 0.26 s, side by side |
| taskscaler sees them | 1.0 s | at once |
| SSH and `docker version` answer | 1.1 s | 0.25 s |
| the waiting job takes a ready VM | 1.85 s | 30 ms |
| total | 6.0 s | 1.0 s |
| the JIT runner registered, after the VM | 0.65–0.8 s | none: registered while the VM is taken, the runner starts 80–160 ms after it |

One restore, 460 ms before: 100 ms waiting for the tap device (looked for
every 100 ms), then the identity script, where `ssh-keygen -A` took 86 ms
and `hostnamectl` 50 ms; each process right after a restore is slow to
start, its memory read in from the snapshot. After: 250–260 ms, with the
tap looked for every 5 ms, the host key made by the plugin, and fewer
processes.

A restored VM's first SSH session took 650 ms to open: 470 ms of
update-motd.d's scripts, some of them Python, and 170 ms starting the
ubuntu user's systemd instance. With the scripts off and the user lingering,
both done in the template before its snapshot, it opens in 18 ms.

What changed: taskscaler's acquire delay is 100 ms (it was 2 s), a class
on this host is listed every 250 ms while VMs are expected (cloud classes
stay at 1 s), the readiness check skips `cloud-init status` once cloud-init
has written its result, fleetingd restores a burst in parallel, and the
runner is registered with GitHub while its VM is taken.

The rest is GitHub's: 3 s for the runner to start and connect, 2.3 s for
GitHub to hand it the job. Of the 3 s, 2 s is `run.sh` until it is
connected, waiting on GitHub; reading the runner's files into the
snapshot's page cache beforehand changed nothing.

## Go build cache

caddy v2.10.2 built with `go build ./cmd/caddy` (Go 1.25, 2,146 cache
entries) in a fresh container each time, as on a single-use VM, after a
one-line change to `caddy.go`. Modules come from a zstd tarball (1.3 s) in
every case but the first. The cache server and S3 (RustFS) on the same
host; "network" adds 5 ms each way and 1 Gbit/s between the job and them.

| Cache | Job | Job, network | Read per job |
|---|---|---|---|
| none | 21–22 s | | |
| GOCACHE as a zstd tarball, trimmed to what the last job used | 6.2 s | 7.8–9.2 s | 125 MB, all of it |
| gobuildcache v1.9.0, GOCACHEPROG straight to S3 (LZ4) | 5.4 s | 6.2–6.3 s | 194 MB |
| fleetci-gocache, through its server (S2) | 5.5–5.6 s | 6.6–7.0 s | 194 MB |

A second build in the same container takes 1.2 s: the rest is fetching.
The tarball also costs 0.6 s to save, every job; with GOCACHEPROG a job
uploads only the entries it built, 1.5 MB here. fleetci-gocache's 0.4 s
over gobuildcache on the network is its server's hop. Without compression
it read 539 MB and took 7.2 s.
