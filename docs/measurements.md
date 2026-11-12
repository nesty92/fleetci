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

## A cache disk per VM

Each job's VM gets a second disk for caches: a qcow2 overlay on the cache
the last job left, kept after the job as the next layer. Docker's and
containerd's data and the Go caches go on it. Measured with
[hack/cachelab](../hack/cachelab): real commits, oldest first, each in a
fresh 4-vCPU, 6 GB VM, running its repo's own CI steps.

fleetci, last 8 commits (`go vet`, `go test -race`, GoReleaser snapshot):

| Job | Image pull | Vet | Tests | GoReleaser | Steps |
| --- | --- | --- | --- | --- | --- |
| no cache disk, first and newest commit | 36–44 s | 77–80 s | 110–119 s | 229–232 s | 462–465 s |
| first on the cache disk, fills it | 32 s | 77 s | 110 s | 221 s | 440 s |
| warm, the 7 commits after | 2.4–2.9 s | 2.3–3.6 s | 29–53 s | 18–20 s | 52–74 s |

fleetingd, last 8 commits (cross-build for 3 architectures):

| Job | Image pull | Build | Steps |
| --- | --- | --- | --- |
| no cache disk | 16–17 s | 48–49 s | 64–66 s |
| warm | 1.3 s | 3.5–5.2 s | 4.8–6.5 s |

Every test time includes 20 s of one `internal/vmjob` test, which waits
for a killed process that a container with no init never reaps; on
runners, and with `docker run --init`, it passes in 3 s.

The disk itself: an overlay takes 0.02 s to make, mounting it and moving
Docker onto it 0.7–0.9 s. The first job fills 15 GB for fleetci (mostly
GoReleaser's builds for every platform) and 2.4 GB for fleetingd; each
warm job adds 33–330 MB and 11 MB. A job on a 10-layer chain ran as fast
as on the merged file; merging 6 layers takes about 4 s. Cloud Hypervisor
refuses a chain deeper than about 16 ("Maximum disk nesting depth
exceeded"), so a chain has to be merged before that.

caddy, on a 2-vCPU, 4 GB VM: the Docker build 79 s cold, 1.3 s warm, 6.1–6.7 s
after a one-line change; the Go build 64 s cold, 4.5–5.2 s warm.

### A merge train

fleetci's last 13 commits as a busy repo's merge train
([hack/cachelab/train.sh](../hack/cachelab/train.sh)): the oldest is main
as the train starts, each other one a merge request whose pipeline runs on
main plus the ones ahead of it. Three pipelines at once, each in a 2-vCPU,
4 GB VM on the same host. A merge request merges once it and all ahead of
it are done, and the next one starts on the cache of the last one merged,
so three VMs read one parent layer at once and none reads a cache from a
merge request that hasn't merged.

| | Per merge request | 12 merge requests |
| --- | --- | --- |
| cache disk | 40–110 s | 357 s |
| no cache | 873–935 s | about 60 min (3 measured; 4 rounds of them) |

Warm, a pipeline's tests took 3–63 s: Go reuses the results of packages a
commit didn't touch. Filling main's cache took 12 min at 2 vCPU (13.4 GB);
each merge request then added 9–335 MB.

### Ten languages

Six commits of a real project per language, oldest first, through one chain
of cache layers ([hack/cachelab/matrix.sh](../hack/cachelab/matrix.sh),
projects and commits in [matrix.txt](../hack/cachelab/matrix.txt)). Each job
runs in a fresh 4-vCPU, 6 GB VM; images come from a pull-through mirror on
the same host, so the pulls' gain is a lower bound. The times are the pull
and the project's CI steps: cold is a job with no cache disk, cached the
median of jobs 2–6.

| Language | Project | Cold | Cached | Saved | Pull | Cache disk |
| --- | --- | --- | --- | --- | --- | --- |
| Go | fleetci | 434 s | 23 s | 94% | 13 → 2.7 s | 15.5 GB |
| Node.js | express | 21 s | 6.9 s | 67% | 7.1 → 1.5 s | 1.8 GB |
| Rust | ripgrep | 27 s | 9.3 s | 65% | 8.3 → 1.5 s | 3.9 GB |
| Python | flask | 20 s | 9.0 s | 55% | 6.7 → 1.5 s | 2.1 GB |
| PHP | commonmark | 11 s | 6.4 s | 53% | 3.0 → 1.5 s | 0.4 GB |
| Ruby | rack | 28 s | 15 s | 46% | 7.0 → 1.5 s | 1.9 GB |
| Java | gson | 69 s | 44 s | 35% | 4.1 → 1.5 s | 1.2 GB |
| Next.js | vercel/chatbot | 45 s | 34 s | 28% | 6.8 → 1.5 s | 3.7 GB |
| TypeScript | zustand | 34 s | 27 s | 23% | 6.9 → 1.5 s | 2.9 GB |
| React | excalidraw | 200 s | 162 s | 19% | 7.5 → 1.8 s | 5.0 GB |

The cache speeds up what downloads or compiles: installs (React 37 → 6.2 s,
Node.js 11 → 2.3 s, Ruby 10 → 3.0 s), Rust's build (8.4 → 2.0 s), and Go's
vet, tests and release (77 → 1.9 s, 118 → 5.5 s, 227 → 13 s), whose build
cache keeps compiled packages and test results. It does nothing for steps
that run the project's own code each time: linters, type checks and
JavaScript test suites ran as long cached as cold. Next.js's build kept
`.next/cache` on the disk and still took 25–26 s either way.

flask's oldest commit fails its own tests, with a cache disk and without;
its cold time is the newest commit's.

### Ten languages, as real CI jobs

The same projects and commits as real jobs on both forges
([hack/cachelab/realmatrix.sh](../hack/cachelab/realmatrix.sh)): a
`workflow_dispatch` on GitHub, a pipeline started by a push on GitLab, each
picked up by fleetci, run by GitHub's runner or gitlab-runner in a fresh
12-vCPU, 12 GB VM, one job at a time. Per language: the oldest commit with
no cache disk, the six with it, the newest with none again. Cold is the
mean of the two with none, warm the median of the last five with the
disk; the first one with it fills it, and the second still fills some.
Every job's line is in [results](../hack/cachelab/realmatrix/results), and
[summary.py](../hack/cachelab/realmatrix/summary.py) makes these tables
from them.

| Language | GitHub cold | GitHub warm | Saved | GitLab cold | GitLab warm | Saved |
| --- | --- | --- | --- | --- | --- | --- |
| Go (fleetci) | 268 s | 31 s | 89% | 132 s | 8.2 s | 94% |
| Node.js (express) | 24 s | 8.0 s | 67% | 13 s | 5.8 s | 54% |
| Rust (ripgrep) | 30 s | 12 s | 60% | 17 s | 10 s | 40% |
| Python (flask) | 23 s | 11 s | 53% | 13 s | 8.9 s | 29% |
| Ruby (rack) | 31 s | 17 s | 46% | 22 s | 15 s | 31% |
| PHP (commonmark) | 14 s | 7.5 s | 45% | 9.2 s | 5.8 s | 38% |
| TypeScript (zustand) | 37 s | 27 s | 28% | 23 s | 22 s | 3% |
| Next.js (vercel/chatbot) | 47 s | 35 s | 25% | 35 s | 32 s | 8% |
| React (excalidraw) | 155 s | 117 s | 25% | 141 s | 115 s | 18% |
| Java (gson) | 74 s | 58 s | 22% | 65 s | 48 s | 26% |

On GitHub a job's steps pull their images, timed in its total: 3–15 s cold,
1.3–2.6 s warm, from Docker's data on the cache disk. On GitLab the job's
image is pulled by gitlab-runner before the script and isn't in the total,
so the totals are smaller and the saving too. GitLab's Go job has no
GoReleaser step, which runs in its own image (on GitHub it builds two
binaries at a time; one per vCPU outgrew 12 GB). flask's two oldest
commits fail their own tests on both forges, the first with a cache disk
included.

From the job queued to its first step: 14–26 s on GitHub, most of it
GitHub's poll and its runner registering and connecting; 9–25 s on GitLab,
where fleetci asks for a job while a VM can take it. The VM itself was
ready in 1–2 s. Before fleetci stopped holding room for a GitLab request
for a job, a GitHub job could wait 40 s more behind it.

### Deduplication and compression

How much a worker's cache disks would shrink on a filesystem that
deduplicates or compresses, from the ten languages' layers (38 GB, all
six layers of each), with
[hack/cachelab/dedup.py](../hack/cachelab/dedup.py):

| Dedup block | Left | Saved | Of it, across projects |
| --- | --- | --- | --- |
| 4 KiB (btrfs with bees or duperemove) | 22.1 GB | 42% | 46% |
| 64 KiB | 31.3 GB | 18% | 65% |
| 128 KiB (ZFS's default record) | 34.3 GB | 11% | 67% |

Compression (zlib level 1, a lower bound for zstd) takes 56% off what
deduplication leaves. Half of what 4 KiB deduplication saves is between
different projects: base images and packages they share. A worker with more
projects on similar stacks would save more. The other half repeats within
one project's chain of layers.

Compression saves more than deduplication, and costs less: btrfs's
`compress=zstd` needs no daemon and no memory for a block table. With both
the 38 GB would be about 10 GB. ZFS deduplication finds little at its
record size and keeps its table in memory. fleetci needs no change for
either; `vm_cache_directory` goes on such a filesystem. Not measured yet:
how jobs run with their layers on btrfs, where copy-on-write fragments
files that VMs write to.
