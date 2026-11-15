# Is each job's class the right size?

fleetci measures every job on its VM and says whether its class fits it:
too small (the job waited for CPU or memory), too big (most of the VM sat
unused), waiting on its disk, or right. It keeps each job's runs, so a job
that needs more as its project grows shows up before it gets slow or runs
out of memory.

## How it measures

When a job gets its VM, fleetci records the VM kernel's counters and starts a
small shell sampler there. Once the job ends, before the VM goes, it reads
them again:

- **Pressure stall information** (PSI, `/proc/pressure/{cpu,memory,io}`):
  the share of the job's time that some task was stalled waiting for a CPU,
  for memory (reclaim, swap-in, page cache refaults) or for the disk. This
  is the signal a CPU or memory average misses: a build on 2 vCPU that keeps
  them 100% busy looks the same on CPU use whether it needed 2 or 8, but on
  PSI the second one waits 97% of the time.
- **CPUs busy**, on average and at the busiest 5 seconds (`/proc/stat`).
- **Peak memory in use**, every second: `MemTotal - MemAvailable`, so page
  cache the kernel can drop doesn't count.

The VM runs one job, so the whole VM is the job's. On a class that reuses
VMs (`capacity_per_instance` or `max_use_count` above 1), the figures cover
every job on the machine at the time.

The sampler costs one shell loop waking once a second, and two SSH sessions
per job. It stops when the job's figures are read, and after 12 hours in any
case.

## The verdicts

| Verdict | When | Suggests |
| --- | --- | --- |
| `undersized` | CPU wait ≥ 20% of the job's time | 2× the CPUs from 50% on, else 1.5× |
| `undersized` | memory wait ≥ 5%, all tasks stalled on memory ≥ 1%, or memory in use ≥ 90% | 1.5× the memory |
| `undersized` | disk wait ≥ 10% with memory ≥ 80% in use: files read again once dropped from the cache | 1.5× the memory |
| `io-bound` | disk wait ≥ 30%, memory not short | nothing: a bigger class won't help |
| `oversized` | at most half the CPUs busy at peak and CPU wait < 5% | the peak + 25% |
| `oversized` | at most half the memory in use and memory wait < 1% | the peak + 50%, whole GB |
| `fits` | none of the above | |
| `short` | the job ran under 30 s | not judged |

When one dimension is too small and the other too big, only the small one
is suggested: change one thing at a time. A one-vCPU or 1 GB class is never
called too big.

## Over time

Each run is compared with the median of the job's last 10 judged runs (once
it has 3). The run's log line is a warning, and `fleetci usage` shows a
trend, when:

- its memory peak, or the CPUs busy at its peak, grew by a quarter;
- its CPU or memory wait rose by 10 points;
- it took a quarter longer;
- it was undersized in most of its last 5 runs.

A job is its project (GitLab) or repository (GitHub) and its name, such as
`acme/api build` or `group/app test 2/4`.

## Where to see it

Each job logs one line, `job usage`, with the figures, the verdict and its
advice: a warning when the class is too small or the job is outgrowing it.

```text
[WARN]  fleetci.usage: job usage: forge=github job="acme/api build" class=2vcpu-4gb fit=undersized
  advice="undersized: tasks waited for a CPU 97% of the time; try 4 vCPU" seconds=41 cpus=2 cpu_peak=2 ...
```

`fleetci usage` lists the jobs, the ones to look at first on top:

```text
$ fleetci usage
FORGE   JOB             CLASS      RUNS  CPU PEAK  MEM PEAK    LAST
github  acme/api build  2vcpu-4gb  14    2.0/2     0.5/4 GB    undersized: tasks waited for a CPU 97% of the time; try 4 vCPU
gitlab  g/app test 1/2  4vcpu-8gb  9     3.1/4     6.2/8 GB    fits; trend: memory peak grew from 4.8 GB to 6.2 GB
gitlab  g/app lint      4vcpu-8gb  31    0.9/4     0.6/8 GB    oversized: at most 0.9 of 4 CPUs busy; used at most 0.6 GB of 8 GB memory; try 2 vCPU, 1 GB
```

`-runs` prints every run with its waits, and `-job` picks jobs by name.

Runs are kept a line of JSON each in `usage_file` in fleetci.toml, by
default `usage.jsonl` in `state_dir` (`fleetci serve`) or the controller's
`state_dir` (a fleet). Without either, as under gitlab-runner's plugin
without `usage_file`, jobs are measured and logged, and their history lasts
until fleetci restarts.

## Checked on a real VM

A 2 vCPU / 4 GB Cloud Hypervisor VM (Ubuntu 26.04), each workload about 40 s,
measured by the same code fleetci runs:

| Workload | Verdict | Figures |
| --- | --- | --- |
| 8 CPU workers | undersized, try 4 vCPU | CPU wait 97% |
| 3.3 GB held, files read | undersized, try 6 GB | memory 3.6 of 3.8 GB, disk wait 21%, memory wait 0.5% |
| 1 worker at 30%, 300 MB | oversized, try 2 GB | memory 0.7 of 3.8 GB, CPU peak 1.3 of 2 |
| 1 CPU and 1 memory worker, 2.3 GB | fits | CPU wait 0.9%, peak 2.0 of 2 |
| synced 4k random writes | io-bound | disk wait 48% |

A 3-thread workload first meant as "right-sized" was called undersized,
with CPU wait 48%: it was right. Three busy threads on 2 vCPU wait half the
time.

The memory squeeze shows why memory isn't judged on memory wait alone:
with 3.3 GB held on 4 GB, the kernel kept dropping and re-reading file pages,
which PSI counts as disk wait (21%), not as memory wait (0.5%).
