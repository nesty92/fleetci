# What fleetci costs next to GitHub-hosted runners

fleetci on AWS spot runs a Linux CI job for about a tenth of what GitHub's
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
  boot included. Each VM also has its disk (30 GiB gp3), and the Runner
  Manager is a fixed cost.

## Boot time

Not yet measured with a baked image. The boots seen so far ran a boot script
that installed Docker and actions/runner, which a baked image no longer does.
Published EC2 figures for a ready image: about 7–8 s from RunInstances to
running, then a few seconds to SSH (2.8 s for a tuned Ubuntu 24.04, 13 s for
a stock one on a t3.micro). Add the Auto Scaling group's launch, the plugin's
polling and fleetci waiting for cloud-init and Docker, and a baked image should
start a job in about 20–40 s. The figures below take 20–30 s, and show 90 s as
a worst case.

Boot time hardly moves the cost, because EC2 bills at least 60 s anyway: a
job that ends in 20 s pays for a minute either way. It moves the wait.

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
| fleetci VM minutes per job | 7.53 (20 s boot) to 8.58 (90 s boot) |

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
| fleetci, m7i.large spot (2 vCPU, 8 GiB), 20–30 s boot | $0.0033–0.0034 | 9.6–9.8× cheaper |
| the same, 90 s boot | $0.0039 | 8.2× cheaper |
| fleetci, m7i.large on-demand, 30 s boot | $0.0099 | 3.3× cheaper |

Larger sizes, using every job's durations and the older 45–90 s boot, so
slightly conservative:

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
fleetci $0.0034 with a 30 s boot):

| Downloaded per job | Through NAT | Public subnet |
|---|---|---|
| 0.2 GB | $0.0126 (2.6× cheaper) | $0.0038 (8.4× cheaper) |
| 0.5 GB | $0.0264 (1.2× cheaper) | $0.0038 |
| 1 GB | $0.0494 (1.5× **dearer**) | $0.0038 |

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
costs about $0.0003, against $0.002 on `ubuntu-slim`. The price is fine; the
wait is not. Even a 20–40 s boot is long for a 10 s check. A warm VM
(`idle_count`) removes the wait, at the cost of its idle hours. A container
per job on a shared VM, as `ubuntu-slim` does, would remove it without paying
for idle time, but gives up a VM per job.

## Spot across clouds

The same Linux 2-core job (5.39 VM minutes with a 30 s boot, disk included)
on 2-vCPU spot VMs near Stockholm, 2026-11-04. Each cloud has an official
fleeting plugin (AWS, Azure, Google Cloud), so each can back a fleetci class.

| Cloud, region | Type | vCPU / GB | Per hour | Per job | Against GitHub |
|---|---|---|---|---|---|
| Azure, Sweden Central | D2pls_v5 spot (Arm) | 2 / 4 | $0.0135 | $0.0015 | 21× cheaper |
| Azure, Sweden Central | D2als_v6 spot | 2 / 4 | $0.0159 | $0.0017 | 19× cheaper |
| Azure, Sweden Central | D2as_v5 spot | 2 / 8 | $0.0170 | $0.0018 | 18× cheaper |
| GCP, europe-north1 | e2-highcpu-2 spot | 2 / 2 | $0.0157 | $0.0017 | 19× cheaper |
| GCP, europe-north1 | e2-standard-2 spot | 2 / 8 | $0.0213 | $0.0022 | 15× cheaper |
| AWS, eu-north-1 | m7g.large spot (Arm) | 2 / 8 | $0.0182 | $0.0019 | 17× cheaper |
| AWS, eu-north-1 | c7a.large spot | 2 / 4 | $0.0234 | $0.0024 | 13× cheaper |
| AWS, eu-north-1 | m7i.large spot | 2 / 8 | $0.0342 | $0.0034 | 10× cheaper |
| OVH | d2-4, shared vCPU, on-demand | 2 / 4 | €0.0198 | $0.0020 | 16× cheaper |

Compute is cheapest on Azure spot today, with GCP close behind, and AWS's
Intel type is the dearest of the three. The differences are a tenth of a cent
per job; network decides more. AWS, Azure and GCP all charge for NAT and
egress, while OVH includes traffic. Spot prices change daily and differ
between regions and zones: GCP's n4-standard-2 is $0.0436/h in Finland and
$0.0137/h in Belgium.

## Against other CI services and OVHcloud

The same workload, one Linux 2-core job (5.37 billed minutes where a service
rounds to the minute, 4.78 where it bills by the second, 5.39 VM minutes for
fleetci with a 30 s boot). Euro prices at $1.15 per euro, an assumed rate.

| Option | Per job | Against GitHub |
|---|---|---|
| GitLab.com small, 2 vCPU 8 GB ($0.01/min) | $0.0478 | 1.5× dearer |
| Buildkite hosted, 2 vCPU ($0.008/min) | $0.0430 | 1.3× dearer |
| GitHub-hosted 2-core ($0.006/min) | $0.0322 | |
| CircleCI medium (10 credits, $0.006/min) | $0.0322 | same |
| Depot 2 vCPU ($0.006/min, per second) | $0.0287 | 1.1× cheaper |
| Blacksmith, WarpBuild 2 vCPU ($0.004/min) | $0.0215 | 1.5× cheaper |
| Namespace 2 vCPU ($0.002–0.003/min) | $0.0134 | 2.4× cheaper |
| Ubicloud 2 vCPU ($0.00125/min) | $0.0067 | 4.8× cheaper |
| fleetci, OVH b3-8 VM, 2 vCPU 8 GB (€0.0512/h) | $0.0053 | 6.1× cheaper |
| fleetci, OVH c3-4 VM, 2 vCPU 4 GB (€0.0457/h) | $0.0047 | 6.8× cheaper |
| fleetci, AWS m7i.large spot | $0.0034 | 9.5× cheaper |
| fleetci, OVH d2-4 VM, 2 shared vCPU 4 GB (€0.0198/h) | $0.0020 | 16× cheaper |
| fleetci, OVH ADVANCE-1 bare metal with Firecracker, 50% busy | $0.0049 | 6.6× cheaper |
| the same, 100% busy | $0.0024 | 13× cheaper |

**OVHcloud Public Cloud VMs** bill per second with a one-minute minimum, like
EC2, so a fresh VM per job works the same way. They are on-demand, with no
spot interruptions, and traffic is included: no NAT charge and no egress
charge, so the network table above does not apply. That makes OVH cheaper
than AWS for jobs that download a lot. OVH Public Cloud is OpenStack, so a
fleetci class there needs a fleeting plugin for OpenStack. d2 instances share
their vCPUs.

**OVHcloud bare metal with Firecracker** is the other model: a fixed monthly
server split into microVMs. ADVANCE-1 (EPYC 4244P, 6 cores, 12 threads, 32 GB,
€114.99 a month) holds six 2-vCPU microVMs and costs as much as about 4,100
GitHub Linux jobs a month. At most it runs about 55,000 jobs a month. Its cost
per job depends only on how busy it is (25% busy: $0.0097, still 3.3× below
GitHub), and in exchange:

- a microVM boots in a second or two, with no 60 s minimum, which suits the
  smallest jobs;
- images and caches can stay on the host's NVMe between jobs, so a job
  downloads little;
- traffic is included;
- capacity is fixed: beyond six jobs at once they wait, unless another class
  (OVH or AWS VMs) takes the overflow.

fleetci has no fleeting plugin for Firecracker yet; this is a design, not a
feature. OVH Public Cloud VMs expose `vmx`, so Firecracker can also run inside
them, though OVH warns that live migration can then crash the guest.

OVHcloud raised its prices twice in 2026, and its own posts disagree on
whether hourly instance prices changed in October; check the price list.
The other services' rates come from third-party comparisons, checked against
the vendors only for GitHub and GitLab.

## Fixed costs

| | Per month |
|---|---|
| Runner Manager (t3.nano, 8 GiB) | $4.58 |
| NAT gateway, if there is none already | $33.58 |

With Linux 2-core jobs ($0.0322 on GitHub, $0.0034 on fleetci), fleetci pays
for itself from about 159 jobs a month, or about 1,325 if it needs a NAT
gateway of its own.

| Linux jobs a month | GitHub | fleetci (spot, no NAT) |
|---|---|---|
| 1,000 | $32 | $8 |
| 10,000 | $322 | $39 |
| 100,000 | $3,220 | $345 |

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
- [GitLab compute minutes](https://about.gitlab.com/pricing/faq-compute-minutes/) and [hosted runners on Linux](https://docs.gitlab.com/ci/runners/hosted_runners/linux/)
- CI service rates: [cicdpipelinecost.com platform comparison](https://cicdpipelinecost.com/platform-comparison), [craci.com runner comparison](https://craci.com/compare/github-actions-runner-providers)
- OVHcloud: [Public Cloud prices](https://www.ovhcloud.com/en-ie/public-cloud/prices/), [price changes](https://blog.ovhcloud.com/en/posts/pricing-evolution-of-public-cloud-bare-metal-and-vps-at-ovhcloud/) and [the October 2026 update](https://blog.ovhcloud.com/en/posts/public-cloud-pricing-update-october-2026/), [billing per second](https://help.ovhcloud.com/csm/en-public-cloud-compute-billing-options?id=kb_article_view&sysparm_article=KB0050466), [nested virtualization](https://support.us.ovhcloud.com/hc/en-us/articles/22118850832787-Public-Cloud-FAQ), [dedicated servers H2 2026 prices](https://blog.ovhcloud.com/en/posts/dedicated-servers-pricing-update-h2-2026/)
- Azure: the [Retail Prices API](https://prices.azure.com/api/retail/prices), queried 2026-11-04
- GCP spot: [gcloud-compute.com](https://gcloud-compute.com/e2-standard-2.html), a third-party table built from Google's SKUs; Google's own [Spot VM pricing](https://cloud.google.com/spot-vms/pricing)
- [EC2 Mac instances](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-mac-instances.html) and [dedicated host pricing](https://aws.amazon.com/ec2/dedicated-hosts/pricing)
