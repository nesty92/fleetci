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

### On a fleet of machines

A controller and two workers in the cachelab container, each worker with
6 CPUs, 7 GB and a 10 GB cache budget, and a class of 4 CPUs and 6 GB with
`cache_disk`: one VM per worker. GitLab, the express repo of the language
matrix, `FLEETCI_CACHE_KEY=matrix-node`:

| Job | Worker | Why | Install | Total |
| --- | --- | --- | --- | --- |
| First, w1 drained | w2 | the only one Ready | 9.1 s | 12.8 s |
| Next commit, both Ready | w2 | holds `matrix-node` | 2.4 s | 5.9 s |

Without the cache, the second job would have gone to w1, which comes first
when both are empty. GitHub, `cache.yml` on WireZone/fleetci-playground:
its two jobs ran one on each worker, each writing `main` on its own worker.
On the next run, `docker pull node:24-alpine` took 1.35 s instead of 7.1 s
on the worker whose copy had it; `postgres:17-alpine`, pulled on the other
worker, took 6.5 s again: workers didn't copy caches to each other yet.

Copying between workers, the same fleet and `cache.yml`. Both workers are
in one container, so their link is the host's memory: what it shows is
what a copy costs on the wire, and the time without a network in the way.

| Copy | Layers | On the wire | Took |
| --- | --- | --- | --- |
| w1's `main` to w2, which had another copy | 666 MB | 395 MB | 0.97 s |
| w2's newer one back to w1 | 665 MB | 395 MB | 0.88 s |
| The next two jobs' new layers, each | 2.4–3.9 MB | 0.1 MB | 16–25 ms |
| The whole chain to an empty w2, uncompressed (twice) | 677 MB | 677 MB | 0.70 s |
| The same, S2 on two cores (twice) | 677 MB | 396 MB | 0.89 s |

With w1 drained, both jobs ran on w2 from the copied chain, and pulled
their images in 1.1 s each, as on w1. At 1 Gbit, the whole chain would
take about 5.8 s uncompressed and 3.4 s with S2; at 10 Gbit about the
same either way, as S2 on two cores keeps up with about 760 MB/s.
`qemu-img check` found the copied chain whole, its backing files at the
receiving worker's paths.

### actions/cache through the disk

The same fleet, GitHub, a throwaway workflow on WireZone/fleetci-playground
with three jobs: `actions/cache` of 200 MB on the VM, of 100 MB in a
`node:24-bookworm-slim` job container, and a BuildKit build of an 80 MB
layer with `cache-to: type=gha,mode=max`. Each job on a disk of its own;
the second run on the same branch:

| Job | First run | Second run | Second run's cache line |
| --- | --- | --- | --- |
| On the VM | 20 s | 43 s¹ | 1 restored from the cache disk (200 MB) |
| In a job container | 160 s | 17 s | 1 restored from the cache disk (100 MB) |
| BuildKit, `type=gha` | 138 s | 25 s | layer restored from the disk, `#6 CACHED` |

¹ Both runs on w1. The restore itself took about 2 s; the rest of the
second run's time was outside the cache steps, and one run doesn't say
where.

Restores from the disk took 1–2 s for 100–200 MB. Before each job had its
own disk, jobs of one run on one branch shared one: on the second run the
VM job found the container job's disk, which had come over from the other
worker, and missed. A download from GitHub's cache in 8 MiB ranges, 8 at a
time, took 16.6 s for the 200 MB archive, against 14–16 s for one stream:
the host's internet link was the limit.

### A merge train on both forges

A merge queue played on both playgrounds, whose plans have none
([hack/cachelab/train/drive.py](../hack/cachelab/train/drive.py)): main
gets a base commit, then 5 cars, 2 at once, each a commit on the car
ahead pushed to `train/<run>/car-<n>`. A car merges, pushed to main, when
it passes and the car ahead has merged, and main's pipeline runs on it.
Each car and each main commit runs two jobs, Go (vet and tests of fleetci)
in `golang:1.27` and Next.js (install, lint, build of vercel/chatbot) in
`node:24`, each keeping its tools' caches as a project would: with
`actions/cache` on GitHub, `cache:` on GitLab; about 990 MB for Go,
230–480 MB for web. The same fleet of a controller and two nodes, a VM of
4 CPUs and 6 GB each; 22 jobs a train with main's base. GitLab's runner
takes up to 2 jobs at once, its cache in the lab's S3 (RustFS).

| | 5 cars merged | Main done | Job time¹ | Go job² | Web job² |
| --- | --- | --- | --- | --- | --- |
| GitHub, no cache disk | 1677 s | 2153 s | 3748 s | 245 s | 135 s |
| GitHub, disk + GitHub's cache | 1443 s (−14%) | 1886 s | 3157 s (−16%) | 220 s | 106 s |
| GitHub, disk + bucket | 1009 s (−40%) | 1412 s | 2244 s (−40%) | 140 s | 89 s |
| GitLab, S3 only | 728 s | 878 s | 2197 s | 95 s | 132 s |
| GitLab, disk + S3 | 682 s (−6%) | 812 s | 1543 s (−30%) | 67 s³ | 108 s³ |

¹ Every job's wall time after main's base, added up. ² Mean wall time on
the cars. ³ Over 3 cars: the driver lost the job records of cars 2 and 5,
whose pipelines passed.

The jobs' own steps took about the same each way (Go 5–50 s: its tests
reuse the results of packages a commit didn't touch; web 37–42 s); the
rest of a job is its VM, image pull, checkout and the cache's restore and
save. On GitHub, 13 restores of the 20 came from the job's own disk; the
others found it empty and downloaded Go's 986 MB from the store behind it, in 62–84 s from
GitHub's cache over the host's internet link and in 2.3–2.9 s from the
bucket, here on the same host as the nodes: the best case for a bucket,
whose case is one in the nodes' own network or region. The disk was
empty for two reasons. The lab's host was 98% full, so its nodes, keeping
8 GB free (`min_free`), evicted main's disks between jobs; and a job that
reached the other node within 30 s of main's job came before the copy,
which nodes make every 30 s. On a host with room, most of these 7 would
have restored from the disk. After each job
the node uploaded its saves to the bucket, none failed. On GitLab, a car's
disk starts from main's; gitlab-runner still downloaded each job's
`cache:` from S3 as well, so the disk saved image pulls and the work in
the VM but not the download: making the disk GitLab's local copy is open
([design](design/cache.md)).

All 115 VMs were ready under 10 s after their restore. Before fleetci's
nodes claimed their VM subnets, the lab's two nodes, sharing one network
namespace, picked the same ones and dialed each other's VMs: 24–252 s to
ready, and a GitLab job failed when its three SSH tries were reset.

### Compressing the caches

In the merge train above, most of the time a GitHub Go job spent saving
its cache went to gzip: `actions/cache` compresses with zstd only when the
step finds it, and the step runs in the job's container (`golang:1.27`,
`node:24`), which has none. gzip then compresses on one core: 71 s of a
74 s save of 986 MB, the upload 2 s. fleetci now lends the VM's zstd to
the job's container (the runner's externals, which containers get at
`/__e`, with the libraries and loader it needs) and puts it on the job's
PATH. The same job, on a 4-CPU VM: `tar --use-compress-program zstdmt`,
8 s to compress 3.4 GB of Go caches into 909 MB, 2 s to upload, 10 s in
all.

GitLab's helper zips with deflate on one core by default. Its own settings
change that, in `[[runners]] environment` or a job's variables:

| Helper v19.5.2, 1.3 GB in 25,561 files, 16 cores | Save | Size | Restore |
| --- | --- | --- | --- |
| zip (default) | 1.1 s | 400 MB | 1.1 s |
| zip, `FF_USE_FASTZIP` | 1.1 s | 400 MB | 1.1 s |
| zip, fastzip, level fast | 0.9 s | 432 MB | 1.1 s |
| `tarzstd` | 2.4 s | 338 MB | 3.1 s |
| `zipzstd`, fastzip, level fast | 0.9 s | 400 MB | 1.0 s |

On a busier run the default zip took 3.4 s to save and 2.4 s to restore,
`zipzstd` at level fast 2.3 s and 1.9 s. The lab's runner now sets
`FF_USE_FASTZIP=true`, `CACHE_COMPRESSION_FORMAT=zipzstd` and
`CACHE_COMPRESSION_LEVEL=fast`; the Go job's cache, 3.4 GB in 68,000
files, is stored as 1015 MB of zstd zip entries (method 93), zipped in
9 s on 4 CPUs.

### Downloading actions

A GitHub runner downloads every action a job uses in "Set up job", before
the first step; on a fresh VM nothing is kept from the job before. From
the logs of 110 jobs on the lab (November 2026, the playground's `go` and
`web` jobs, each using `actions/checkout@v4` and `actions/cache@v4`):

| job | resolving (GitHub's API) p50 | downloading p50 | p90 | max | per action | share of the job |
|---|---|---|---|---|---|---|
| go  | 0.51 s | 1.20 s | 1.59 s | 2.79 s | 0.60 s | 0.7% |
| web | 0.51 s | 1.27 s | 2.61 s | 3.28 s | 0.64 s | 1.2% |

The runner can take the archives from a folder instead
(`ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE`, `<owner>_<repo>/<sha>.tar.gz`); it
still asks GitHub to resolve each action, so a disk would save the
download, about 0.6 s per action, not the 0.5 s of resolving.

### GitLab under steady load

Twenty pull requests, one every 150 s, each running the playground's `go`
and `web` jobs and merging to main (which runs them again), on four nodes
of 4 CPUs and 4.5 GB, a cache budget of 10 GB each. One run of each,
November 2026; GitLab's runner zips caches with fastzip and zstd.

| | disk | disk, no copies | bucket only | no cache |
|---|---|---|---|---|
| jobs (failed) | 80 (0) | 78 (2) | 78 (0) | 82 (16) |
| queue p95 | 4 s | 78 s | 88 s | 306 s |
| queued under 30 s | 100% | 87% | 82% | 59% |
| go p50 (p95) | 51 s (103 s) | 53 s (127 s) | 83 s (169 s) | 222 s (331 s) |
| web p50 (p95) | 94 s (132 s) | 108 s (173 s) | 144 s (210 s) | 154 s (251 s) |
| web pull requests p50 | 94 s | 116 s | | |
| caches started empty | 20 of 80 | 27 of 78 | | |
| copied between nodes | 119 GB | 0 | | |

"disk" copied shared caches between nodes; "disk, no copies" ran with
copies off (the default since) and the actions/cache L1 in Go. The
failures are GitLab's: it cancels a pipeline on main still waiting when a
newer push arrives (all 2, and 14 of 16), and Docker Hub's anonymous pull
limit (2 of 16; the lab's VMs pull from Docker Hub directly). In "bucket
only", 39% of the go job is preparing the VM, mostly pulling images, which
the disk keeps.
