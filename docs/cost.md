# What fleetci costs next to GitHub-hosted runners

fleetci on AWS spot runs a Linux CI job for about a ninth of what GitHub's
hosted runners charge. That holds as long as image pulls don't go through a
NAT gateway. It serves only Linux, though, and on GitHub macOS makes up 41% of
the hosted bill.

The baseline is real usage: 104.5M GitHub Actions jobs from 45K public
repositories (100+ stars), February to June 2026, with each job's runner size
taken from its workflow file. Prices are AWS Stockholm (eu-north-1) and GitHub's
list prices on 2026-11-04. [hack/cost](../hack/cost) reproduces every number.

## How each side bills

- **GitHub** rounds each job up to a whole minute: Linux 2-core $0.006/min,
  ARM $0.005, Windows $0.010, macOS $0.062. Public repositories run free on the
  standard runners, so the comparison is for private repositories, or for use
  beyond the included minutes (3,000 a month on Team). Self-hosted runners
  cost nothing on GitHub; a $0.002/min charge for them was announced for March
  2026 and postponed.
- **fleetci** pays EC2 per second with a 60 s minimum for one fresh VM per job,
  boot included. With a baked image the VM takes 45–90 s before the job starts.
  Each VM also has its disk (30 GiB gp3), and the Runner Manager is a fixed
  cost.

## Job durations

From [GHARuns](https://doi.org/10.5281/zenodo.20427454) (`jobs.parquet`): jobs
that succeeded, failed or were cancelled, longer than 0 s and at most 6 h.

| | |
|---|---|
| Jobs | 104,514,882 |
| Median, mean | 135 s, 7.1 min |
| p90, p99 | 17 min, 71 min |
| Under 1 min | 34% of jobs, 1.8% of minutes |
| 10 min or longer | 18% of jobs, 72% of minutes |
| GitHub billed minutes per job | 7.64 (rounding adds 7.8%) |
| fleetci VM minutes per job | 7.85 (45 s boot) to 8.58 (90 s boot) |

GitHub's rounding and fleetci's boot cost about the same, so the per-minute
price decides.

## Runner sizes

GHARuns has no runner labels. The [workflow-histories
dataset](https://doi.org/10.5281/zenodo.20340547) covers the same repositories,
so each job run is matched to the `runs-on` of its job in the workflow's
current file. Matrix values come from the run's name, for example
`(ubuntu-latest, 3.11)`.

This gives a size for 55.7M jobs (53%). The rest are matrix jobs with dynamic
values (31%), reusable workflows (8%), or have no file in the dataset (7%).
3.0M of the matched jobs run on self-hosted or third-party runners. Of the
52.7M on GitHub's runners:

| Tier | Jobs | Share of the hosted bill | fleetci |
|---|---|---|---|
| Linux 2-core | 88.4% | 46.9% | yes |
| macOS | 4.4% | 41.2% | not yet |
| Windows | 4.8% | 8.0% | no |
| Linux 4–64 core, ARM, 1-core | 2.2% | 2.8% | yes |

macOS is a twentieth of the jobs and two-fifths of the money. fleetci can
replace about half of a typical hosted bill: the Linux half.

## Per job

Standard Linux 2-core jobs on their own run shorter (median 73 s, 46% under a
minute) and are billed 5.37 minutes each:

| | Per job | Against GitHub |
|---|---|---|
| GitHub Linux 2-core | $0.0322 | |
| fleetci, m7i.large spot (2 vCPU, 8 GiB) | $0.0035–0.0039 | 8.2–9.3× cheaper |
| fleetci, m7i.large on-demand | $0.0102 | 3.1× cheaper |

Larger sizes, using every job's durations:

| Machine | GitHub | fleetci spot | Against GitHub |
|---|---|---|---|
| 2 vCPU arm64 | $0.0382 | m7g.large: $0.0028–0.0031 | 12–14× cheaper |
| 4 vCPU x64 | $0.0917 | m7i.xlarge: $0.0098–0.0107 | 8.6–9.4× cheaper |
| 8 vCPU x64 | $0.1681 | m7i.2xlarge: $0.0222–0.0243 | 6.9–7.6× cheaper |

Spot prices move, and an interrupted VM means a rerun.

**Burstable types change this.** T3, T3a and T4g launch in unlimited mode with
no launch credits. A CPU-bound job spends surplus credits, which are billed
when the VM is terminated at $0.05 per vCPU-hour: about $0.08/h on a t3.medium at
full CPU, four times its spot price. Standard mode avoids the charge but
starts with no credits, throttled to the baseline (20% of each vCPU on a
t3.medium). Use non-burstable types for classes that compute.

## Network and caches

Every job starts on a fresh VM, so it downloads its images and dependencies
again. GitHub's hosted runners do the same, but there GitHub pays for the traffic:

| Traffic | AWS price (Stockholm) |
|---|---|
| Downloads from the internet | free |
| Through a NAT gateway | $0.046/GB processed, plus $0.046/hour for the gateway |
| Public IPv4 on a VM instead | $0.005/hour per VM |
| Uploads to the internet (artifacts, `actions/cache` saves, registry pushes) | $0.09/GB after 100 GB a month |
| S3 in the same region through a gateway endpoint | free; storage $0.023/GB-month |

The NAT charge is the one that matters. Per Linux 2-core job (GitHub $0.0322,
fleetci $0.0039):

| Downloaded per job | Through NAT | Public subnet |
|---|---|---|
| 0.2 GB | $0.0131 (2.5× cheaper) | $0.0044 (7.3× cheaper) |
| 0.5 GB | $0.0269 (1.2× cheaper) | $0.0044 |
| 1 GB | $0.0499 (1.5× **dearer**) | $0.0044 |

So:

- Put class VMs in public subnets with a public IPv4 (the module's
  `private_address_only = false`). The runner security group still admits
  only the Manager.
- Or pull through ECR, whose layers come from S3, with an S3 gateway endpoint.
- Bake images every job uses into the class image, so no job downloads them.
- GitLab's cache in S3 is free in transfer with a gateway endpoint. GitHub's
  `actions/cache` stays on GitHub, where a restore is a free download and a
  save is a paid upload.
- Behind one NAT address, every VM shares Docker Hub's anonymous pull limit.

## The smallest jobs

46% of Linux 2-core jobs finish in under a minute. For them the cost is small
either way; the boot is what hurts. What others do:

- **GitHub's `ubuntu-slim`** is 1 vCPU in a container, not a VM, at
  $0.002/min, for jobs of at most 15 minutes and without Docker. It is meant
  for labelling, triage and similar automation.
- **Firecracker microVM runners** such as Blacksmith claim boots under
  3 seconds, at $0.004/min for 2 vCPU. Depot publishes 10–45 s from request to
  job start.
- **Self-hosted on spot**, as fleetci does: RunsOn publishes about
  $0.0009/min for 2 ARM vCPUs, plus its licence.

A sub-minute job on fleetci's `1vcpu-2gb` (c7a.medium spot, disk included)
costs about $0.0004, against $0.002 on `ubuntu-slim`. The price is fine; the 45–90 s
boot is not. A warm VM (`idle_count`) removes the wait, at the cost of its
idle hours. A container per job on a shared VM, as `ubuntu-slim` does, would
remove the wait without paying for idle time, but gives up a VM per job.

## Fixed costs

| | Per month |
|---|---|
| Runner Manager (t3.nano, 8 GiB) | $4.58 |
| NAT gateway, if there is none already | $33.58 |

With Linux 2-core jobs ($0.0322 on GitHub, $0.0039 on fleetci), fleetci pays
for itself from about 162 jobs a month, or about 1,350 if it needs a NAT
gateway of its own.

| Linux jobs a month | GitHub | fleetci (spot, no NAT) |
|---|---|---|
| 1,000 | $32 | $8 |
| 10,000 | $322 | $44 |
| 100,000 | $3,220 | $395 |

## macOS, next

macOS is where the money is. fleetci already gives gitlab-runner its VMs
through the nesting protocol, the one GitLab's own macOS runners use, so a
macOS class is an extension of what fleetci has, not a rewrite. GitLab no
longer runs job VMs with Tart: after Tart's license changed (AGPL-3.0 to Fair
Source, with a 100-core limit), nesting gained a `virtualizationframework`
hypervisor that drives Apple's Virtualization.framework directly (merged July
2023, in nesting's releases since). It reads Tart's VM layout, so Tart remains
a tool for building images only.
On AWS that means an EC2 Mac instance on a dedicated host (one per host,
allocated for at least 24 hours; mac2.metal $0.65/h, mac2-m2.metal $0.88/h in
us-east-1), running up to two macOS VMs, as Apple's license allows.

Two VMs on a mac2-m2.metal cost $0.0073 per VM-minute when the host is busy,
against GitHub's $0.062: 8.5× cheaper at full use, about 2.5× at 30%. The host
bills whether or not jobs run, so warm hours count.

## Limits of this comparison

- The repositories are public, with 100+ stars. Pull-request-triggered jobs
  are not in GHARuns.
- A job's size comes from the current version of its workflow file.
- Durations were measured on GitHub's runners; an EC2 type of the same size
  can be faster or slower.
- Network volume per job is not in any dataset; the table above shows the range.

## Sources

- [GitHub Actions runner pricing](https://docs.github.com/en/billing/reference/actions-runner-pricing)
- [GHARuns paper](https://orbi.umons.ac.be/bitstream/20.500.12907/56424/1/main.pdf) and [dataset](https://doi.org/10.5281/zenodo.20427454), version 2026-07-08
- [A dataset of GitHub Actions workflow histories](https://doi.org/10.5281/zenodo.20340547), version 2026-05-22
- [GHALogs](https://zenodo.org/records/14796970), an earlier dataset of run logs
- [GitHub's self-hosted runner charge postponed](https://samexpert.com/github-actions-pricing-backlash-2026/)
- AWS EC2 on-demand and spot prices, public IPv4, data transfer and S3:
  AWS's public price feeds for Europe (Stockholm), 2026-11-04
- NAT gateway: [AWS VPC pricing](https://aws.amazon.com/vpc/pricing/), Stockholm
- [Burstable instances: unlimited mode](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/burstable-performance-instances-unlimited-mode-concepts.html) and [credits](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/burstable-credits-baseline-concepts.html)
- [nesting: Virtualization.framework hypervisor](https://gitlab.com/gitlab-org/fleeting/nesting/-/merge_requests/7) and [why GitLab replaced Tart](https://gitlab.com/gitlab-org/ci-cd/shared-runners/infrastructure/-/issues/97)
- [GitHub's ubuntu-slim runner](https://docs.github.com/en/billing/reference/actions-runner-pricing), and runner vendor prices compared: [cicdpipelinecost.com](https://cicdpipelinecost.com/depot-vs-namespace-vs-blacksmith), [craci.com](https://craci.com/compare/github-actions-runner-providers), [runnerbench.com](https://runnerbench.com/)
- [EC2 Mac instances](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-mac-instances.html) and [dedicated host pricing](https://aws.amazon.com/ec2/dedicated-hosts/pricing)
