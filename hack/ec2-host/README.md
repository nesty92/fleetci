# Measuring microVMs on one EC2 host against one EC2 VM per job

A kit for someone with an AWS account to measure, with released fleetci, whether
GitHub Actions jobs are cheaper and as fast on microVMs packed onto one big EC2
instance with nested virtualization as on one EC2 VM per job. Everything is
created by `tofu apply` and removed by `tofu destroy`; a full session costs
about $15 (see [Cost](#cost)).

```
your machine                           AWS, default VPC, one region
load.py ─ pushes PRs ─► GitHub repo
                          │ queued jobs
                          ▼
                    host: c8i.4xlarge, nested virtualization on
                    fleetci serve (v0.9.1)
                      ├─ host-4vcpu-8gb[-nodisk]  fleetingd ─ microVMs on this host
                      ├─ host-2vcpu-4gb[-nodisk]  fleetingd ─ microVMs on this host
                      ├─ ec2-4vcpu-8gb  fleeting-plugin-aws ─► ASG of c8i.xlarge
                      └─ ec2-2vcpu-4gb  fleeting-plugin-aws ─► ASG of c8i.large
```

One `fleetci serve` has both arms. A run picks its arm by the classes its
jobs ask for, so the arms share the GitHub repo, the region, the CPU family
(Intel Xeon 6, C8i), the software in the VM and fleetci itself.

## What it answers

1. **Cost per job.** MicroVMs on a host paid by the hour, against VMs
   paid per second (one minute minimum) for each job's life. `cost.py`.
2. **Nested virtualization's overhead.** The same jobs' phases (`go vet`,
   `go test -race`, `pnpm install`, `next build`) on a microVM inside the
   host and on an EC2 VM of the same size. AWS recommends bare metal for
   performance-sensitive nested work; this says how much it matters for CI.
3. **Time to a running job.** Queue time: GitHub queuing the job to the
   job starting. On EC2 that includes a VM's boot; on the host, a microVM
   restored from a snapshot.
4. **What cache disks add on the host.** The same microVMs with and without
   each job's own disk of Docker images and caches (`host-*` against
   `host-*-nodisk`).

## The workload

[hack/cachelab/load](../cachelab/load) (`load.py`): a pull request every
150 s on average (Poisson arrivals, seeded by the run's name, so two runs
with the same seed get the same arrivals), 20 by default. Each pull request
is the next of 40 real consecutive commits of two projects, and its
pipeline has two jobs:

| job | class | what it runs |
| --- | --- | --- |
| `go` | a 4 vCPU, 8 GB class | fleetci's own `go vet` and `go test -race` |
| `web` | a 2 vCPU, 4 GB class | vercel/chatbot's `pnpm install`, checks and `next build` |

When a pull request's pipeline passes it merges, and main's pipeline runs.
Each job keeps its tools' caches with `actions/cache`, as a project would.
`load.py` records each job's queue time and steps from GitHub's API in
`$LOAD_OUT/<run>.jsonl`.

## What you need

- An AWS account you may spend about $15 in, an AWS CLI profile for it,
  and the On-Demand vCPU quota for standard instances ("Running On-Demand
  Standard (A, C, D, H, I, M, R, T, Z) instances") of at least 52 in the
  region: 16 for the host and up to 36 for the baseline VMs. New accounts
  often have less; ask in Service Quotas a day ahead.
- `tofu` (or Terraform 1.8+), `aws` (v2), `gh` (logged in), `git`,
  `python3` and `ssh` on your machine.
- A GitHub repository of your own for the load, private or public, with one
  commit on `main` (a README). It gets many `load/…` branches and workflow
  runs; use one made for this. Your machine must be able to push to it over
  SSH (`git@github.com:…`).
- A fine-grained GitHub token for that repository only, with
  *Actions: Read* and *Administration: Read and write* (fleetci registers
  a one-job runner per job). It goes onto the host in step 4, never into
  tofu's files.

## Run it

### 1. Check the region

Nested virtualization is on C8i, M8i, R8i and a few other families, not in
every region. The host's and the VMs' types must both be there:

```sh
export AWS_PROFILE=my-test-account AWS_REGION=us-east-1
aws ec2 describe-instance-types \
  --filters Name=processor-info.supported-features,Values=nested-virtualization \
  --query 'InstanceTypes[].InstanceType' --output text | tr '\t' '\n' | grep '^c8i\.'
```

`c8i.4xlarge`, `c8i.xlarge` and `c8i.large` must be listed. If they aren't,
pick another region, or another listed family with `host_type` and
`vm_classes` (the same family for both, so the CPUs match).

### 2. Bake the baseline VMs' image

```sh
cd hack/ec2-host
./bake.sh            # about 15 minutes; prints ami-…
```

It runs [images/github.json](../../images/github.json)'s commands on Ubuntu
24.04: the recipe of the image the microVMs start from. Both have the same
Docker, git, gh, Python, build tools and actions/runner 2.338.2.

### 3. Create the host and the groups

```sh
cp terraform.tfvars.example terraform.tfvars   # region, profile, your IP as /32, SSH key, repo, the AMI
tofu init
tofu apply
host=$(tofu output -raw host)
```

The host installs everything on first boot, in about 3 minutes:

```sh
ssh ubuntu@$host 'cloud-init status --wait; ls /etc/fleetci'
```

`ready` must be there. `NO-KVM` means the instance has no nested
virtualization: check `host_type` against step 1.

### 4. The token, then start fleetci

```sh
ssh ubuntu@$host 'sudo install -m 600 /dev/stdin /etc/fleetci/github.token' < path/to/token-file
ssh ubuntu@$host 'sudo fleetci check -config /etc/fleetci/fleetci.toml -runner-config ""'
ssh ubuntu@$host 'sudo systemctl start fleetci-serve'
```

`fleetci check` must say `ok` on every line but `gitlab ci`. Each microVM
class downloads the image (1.3 GB) when it starts, and makes its snapshot
for its first VM. So that this isn't measured, warm the arms up first with
short runs whose records you set aside (the variables are in step 5). An
EC2 VM's disk is read from the AMI's snapshot on first touch, on every VM,
not just the first: that is part of what a VM per job costs, and is
measured.

```sh
LOAD_CLASS_GO=host-4vcpu-8gb LOAD_CLASS_WEB=host-2vcpu-4gb $L github disk warm-a 2 30
LOAD_CLASS_GO=host-4vcpu-8gb-nodisk LOAD_CLASS_WEB=host-2vcpu-4gb-nodisk $L github plain warm-b 2 30
LOAD_CLASS_GO=ec2-4vcpu-8gb LOAD_CLASS_WEB=ec2-2vcpu-4gb $L github plain warm-c 2 30
```

### 5. The runs

Run them from the repository's root, one at a time, in this order. Each
takes about an hour. Give each run a new name; the names below are what
the results section expects.

```sh
export LOAD_GITHUB_REPO=me/fleetci-measure       # the repo from "What you need"
export LOAD_OUT=$HOME/fleetci-measure            # where the records go
export LOAD_GIT_NAME="Your Name" LOAD_GIT_EMAIL=you@users.noreply.github.com
export LOAD_METRICS="ssh ubuntu@$host curl -s http://127.0.0.1:9482/metrics"
L=hack/cachelab/load/load.py
```

| run | arm | command |
| --- | --- | --- |
| `ec2-1` | one EC2 VM per job | `LOAD_CLASS_GO=ec2-4vcpu-8gb LOAD_CLASS_WEB=ec2-2vcpu-4gb $L github plain ec2-1` |
| `host-1` | microVMs, no cache disk | `LOAD_CLASS_GO=host-4vcpu-8gb-nodisk LOAD_CLASS_WEB=host-2vcpu-4gb-nodisk LOAD_SEED=ec2-1 $L github plain host-1` |
| `disk-1` | microVMs with cache disks | `LOAD_CLASS_GO=host-4vcpu-8gb LOAD_CLASS_WEB=host-2vcpu-4gb LOAD_SEED=ec2-1 $L github disk disk-1` |
| `ec2-2`, `host-2`, `disk-2` | the same again | as above, `LOAD_SEED=ec2-1` on all three |

`LOAD_SEED=ec2-1` gives every run the same arrivals. The second round
says how much runs differ by chance; differences smaller than that are
noise. With budget for more, a busier round (`$L github plain ec2-3 40 75`
and so on: 40 pull requests, one every 75 s) shows how each arm handles
twice the load: the host's microVMs then queue for room, and the EC2
groups launch more VMs.

A run that fails midway can be run again under a new name; `load.py`
appends to `<run>.jsonl`, so don't reuse one.

### 6. Collect

After each run:

```sh
python3 hack/cachelab/load/summary.py ec2-1 host-1 disk-1
python3 hack/ec2-host/cost.py ec2  ec2-1  --group fleetci-measure-ec2-4vcpu-8gb=0.1874 --group fleetci-measure-ec2-2vcpu-4gb=0.0937
python3 hack/ec2-host/cost.py host host-1 --host-price 0.7497
python3 hack/ec2-host/cost.py host disk-1 --host-price 0.7497
```

(us-east-1 on-demand prices, November 2026; use your region's.)

At the end, copy the host's own records:

```sh
ssh ubuntu@$host 'sudo journalctl -u fleetci-serve --no-pager' > $LOAD_OUT/serve.log
scp ubuntu@$host:/var/log/fleetci-measure/vmstat.log $LOAD_OUT/
```

`vmstat.log` has the host's CPU every 10 s: `us`+`sy` is how busy the
microVMs kept it, `st` what the hypervisor under it took. A host idle
half the time costs twice what it would packed full: say so next to its
cost per job.

### 7. Report

Send `$LOAD_OUT` (the `.jsonl` files, `.metrics-*`, `serve.log`,
`vmstat.log`) and the summaries' output. They hold job logs' cache lines
and timings, no credentials. The results go into
[docs/measurements.md](../../docs/measurements.md) under "EC2: microVMs on a
nested host against a VM per job", as a table per question:

- cost per job, each arm, and the host's mean CPU use during its runs;
- queue time p50/p95, each arm;
- each phase's p50 on `ec2-*` against `host-*` (the nested overhead), and
  `host-*` against `disk-*` (the cache disks);
- the region, instance types, fleetci and image versions, and the dates.

### 8. Clean up

```sh
ssh ubuntu@$host 'sudo systemctl stop fleetci-serve'
tofu destroy
./bake.sh delete ami-…                          # the AMI from step 2 and its snapshot
python3 hack/cachelab/load/load.py clean github load/   # the load's branches
```

Stopping `fleetci-serve` removes its idle VMs; `tofu destroy` removes the
groups with any VM left in them. Revoke the token. Check the EC2 console
for anything tagged `Project = fleetci-measure` that is still there.

## Cost

us-east-1 on-demand, Linux, November 2026; check the
[EC2 pricing page](https://aws.amazon.com/ec2/pricing/on-demand/) for your
region and date.

| | per hour | a session (about 7 h) |
| --- | --- | --- |
| host c8i.4xlarge (16 vCPU, 32 GB) | $0.7497 | $5.25 |
| host disk, 200 GB gp3 | $0.022 | $0.15 |
| baseline c8i.xlarge (4 vCPU, 8 GB) | $0.1874 while a job runs | about $2 for two runs |
| baseline c8i.large (2 vCPU, 4 GB) | $0.0937 while a job runs | about $1 for two runs |
| bake, AMI snapshot (30 GB) | | under $0.50 |

Nested virtualization costs nothing extra. Spot (`use_spot = true`) makes
the baseline cheaper but no longer like for like with the on-demand host;
leave it off for the first round.

## How it's built

- **Host** (`main.tf`): Ubuntu 24.04, `cpu_options { nested_virtualization =
  "enabled" }` (AWS provider 6.33 or later), 200 GB gp3. SSH only from
  `operator_cidr`. Its role has fleeting-plugin-aws's
  [IAM policy](https://gitlab.com/gitlab-org/fleeting/plugins/aws#recommended-iam-policy),
  scaling only the two baseline groups and sending SSH keys only to
  instances tagged as baseline VMs.
- **Host software** (`host-user-data.sh`): fleetci v0.9.1 (checksum
  checked), Cloud Hypervisor v54.0, the fleetingd fork v0.4.0-fleetci.4,
  fleeting-plugin-aws v1.3.1, and `/etc/fleetci/fleetci.toml`. The
  microVMs start from the published image
  [vm-image-github-20261116-23](https://github.com/nesty92/fleetci/releases/tag/vm-image-github-20261116-23),
  pinned. Their traffic to the instance metadata is dropped, so they can't
  read the host's role. fleetci keeps 1 CPU and a tenth of the memory for
  the host: 15 vCPUs and about 28 GB for microVMs, for example three 4 vCPU
  microVMs and one 2 vCPU microVM at once. A job that doesn't fit waits for
  room, which shows in its queue time.
- **Baseline** (`main.tf`): one Auto Scaling group per class, as
  fleeting-plugin-aws asks (min and desired 0, scale-in protection,
  `AZRebalance` suspended), from the baked AMI. No instance profile. SSH only
  from the host, which reaches them at their private addresses with EC2
  Instance Connect keys.
