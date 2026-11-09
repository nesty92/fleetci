# Standalone fleetci on many hosts

Status: plan. The first step, the embedded GitLab runner, is a spike on
`feat/standalone-gitlab`.

## Where this goes

fleetci becomes one program that runs anywhere and takes jobs from GitLab
and GitHub itself, with no gitlab-runner beside it. Several machines,
joined over a VPN, run it together as a federation: they share the
work, and the others keep going when one of them fails.

Today a GitLab install is gitlab-runner plus fleetci's fleeting plugin, and
GitHub jobs need `fleetci serve`, a second process with classes of its own
(two processes on one fleeting group remove each other's VMs). gitlab-runner
also takes a job before anyone knows its class, because the class only
arrives in the job's `FLEETCI_CLASS` variable, so a job that finds no room
waits inside fleetci until `acquire_timeout`.

## Step 1: run GitLab jobs in fleetci's own process

gitlab-runner is MIT-licensed, and what runs a job is in public packages:

- `network.GitLabClient` requests jobs, sends their state and streams their
  logs;
- `common.NewBuild(...).Run(...)` runs one;
- `executors/docker.NewProvider()` is the Docker executor.

fleetci keeps its own copy of gitlab-runner's job loop, `commands/multi.go`
(`feedRunners` → `processRunners` → `processRunner` → `requestJob` →
`processBuildOnRunner`), as close to the original as it can stay, so each
gitlab-runner release can be compared function by function. What fleetci
leaves out: config reload, the session server, usage logs, the boot-verify
canary and the suspend/resume sweep.

### The seam

gitlab-runner's loop asks the executor provider for capacity (`Acquire`)
before it asks GitLab for a job, and the Docker executor runs a job on any
`executors.Environment` it is handed as the job's executor data. Its own
docker-autoscaler is built the same way
(`executors/internal/autoscaler`): `Acquire` reserves taskscaler capacity,
or fails with `NoFreeExecutorError` so no job is requested; once a job
arrives, the executor's `Prepare` acquires the VM and the environment dials
it through fleeting's connector.

fleetci's provider follows that code:

- `Acquire` reserves room for one VM of the runner's class in fleetci's
  host queue, without waiting. No room: `NoFreeExecutorError`, no job is
  requested, and the job stays pending in GitLab.
- `Prepare` creates the VM from the reservation and gives the job a
  `connector.Client` on the VM's one SSH connection, which the job shares
  with the VM's other jobs and its reclaim watch (`docs/design/spot.md`).
- `Release` gives the VM, or the unused reservation, back.

There is no plugin process, no gateway and no nesting socket on this path.

### Compatibility

`[gitlab] config_file` takes gitlab-runner's own `config.toml`, so a runner
that works with fleetci's plugin today runs embedded without changes: all
of `[[runners]]` applies (`[runners.docker]`, `[runners.cache]`,
`environment`, feature flags, `request_concurrency`...), its `autoscaler`
table is ignored, and each job's `FLEETCI_CLASS` picks its class, as with
the plugin. Pipelines don't change. The probe in
[measurements](../measurements.md#embedded-gitlab-runner-against-the-plugin)
ran the same pipeline on both with the same results.

What the embedded loop doesn't have yet:

- the session server, so no interactive web terminal (stock runners have
  none either unless `[session_server]` is set);
- the metrics and debug server;
- config reload on change: a restart reads it;
- graceful stop on `SIGQUIT`: stopping aborts running jobs;
- the CLI around the loop (`register`, `verify`, `unregister`).

What it removes, against the plugin:

- the loopback SSH gateway that poses as gitlab-runner's one instance, and
  the `max_instances = 1`, `capacity_per_instance` and `max_use_count`
  settings it needs;
- the nesting API and `vm_isolation`, with its `class-${FLEETCI_CLASS}`
  image name;
- the proxy that ends vm_isolation's fixed-password login inside fleetci;
- `fleetci watchdog`, since gitlab-runner never restarts a dead plugin;
- `fleetci runner-config` and the `config.toml` checks, which keep those
  settings right;
- two processes on one host budget: GitHub and GitLab share it.

The plugin stays. It is the way to use fleetci with an existing
gitlab-runner install, and none of its code changes.

### Runners

One runner, as today, is the default: the `fleetci` tag, the class in the
job's `FLEETCI_CLASS`, pipelines unchanged. One runner per class, tagged
with the class's name, is an option (`[[gitlab.runners]]`), next to it or
instead of it.

Every way a runner asks for a job (`POST /jobs/request`, the job router's
`GetJob`) also takes it; there is no looking first and no giving back. So a
single runner takes a job before it knows its class, which leaves three
problems:

1. A job of a full class waits inside fleetci, and fails at
   `acquire_timeout` if the class stays full.
2. Jobs waiting count against `concurrent`, so a few large ones can keep
   fleetci from asking for small jobs it has room for.
3. With several hosts, a full host can take a job another host has room
   for.

What answers them:

- Ask for a job only while some class has room (1, 2; done). The runner
  reserves room in the first class that has some, the default class first,
  before it asks; a job of that class takes it, a job of another gives it
  back and waits for its own. A full host takes no jobs, and a nominated
  job's held room stops new ones.
- Run the job on any host's VM (3, once planned for step 2 and left out
  of the federation). The Docker executor only
  dials the VM over SSH, so the host that took the job can run it on a VM
  another host created, over Tailscale, that host carrying the SSH as
  fleetci's proxy does today. The cluster chooses where the VM goes; GitLab
  never needs to know. Nominations keep working, because a taken job's
  class is known. The cost: the Docker API's traffic goes through the host
  that took the job, and the job is lost if that host fails. Image pulls,
  cache and artifacts go from the VM itself.
- List pending jobs with GitLab's API (`GET /projects/:id/jobs?scope=pending`,
  or instance-wide for an admin), to warm VMs and see a large job waiting
  before it is taken. The API shows tags, not variables, so this needs the
  class as a tag too; the template can carry both.

One runner per class answers 1 and 3 without any of that: fleetci asks for
a class's job only when the class has room, so jobs it can't run stay
pending in GitLab, where another host may take them. It costs a
registration per class, the class as the job's tag, and nominations for
jobs still in GitLab, which fleetci can't see or reorder.

### Job sources are plugins, as fleeting's providers are

fleeting keeps the cloud behind a small interface (`InstanceGroup`) and
each cloud in a plugin binary of its own, run over hashicorp/go-plugin, so
the implementation can be swapped without touching what uses it. fleetci
does the same one level up: the core owns the classes, the host budget, the
queue, the VMs and, later, the federation; where jobs come from is a job
source behind an interface, in a binary of its own.

```go
// Capacity is what the core gives a job source.
type Capacity interface {
	Classes(ctx) ([]Class, string /* default */, error)
	Reserve(ctx, class) (Reservation, error)        // room now, or ErrNoRoom
	Acquire(ctx, Reservation) (VM, error)           // the reserved VM
	Release(ctx, Reservation) error
	Create(ctx, class) (VM, error)                  // wait for room
	Conn(vmID) (*proxy.Conn, error)                 // the VM's one SSH connection, shared; never closed by the source
	Delete(ctx, vmID) error
}

// Source takes jobs from a forge and runs them on VMs from Capacity.
type Source interface {
	Init(ctx, config []byte, capacity Capacity, log) error  // its fleetci.toml table, as JSON
	Run(ctx) error  // until ctx ends; then stops its jobs
}
```

Everything else crosses the boundary as data. The VM's SSH connection
stays in the core, shared by every job on the VM and by the reclaim watch;
a source opens sessions and forwards on it, and as a plugin those cross
go-plugin's broker as streams. Capacity reaches the plugin through the
broker too.

Implementations, all swappable:

- `fleetci-source-gitlab`: the embedded gitlab-runner loop of this step.
  gitlab-runner's 250 modules and 80 MB live in it, not in the core, and it
  follows gitlab-runner releases on its own. Another GitLab implementation,
  a native one or a later gitlab-runner, replaces it without a core change.
- `fleetci-source-github`: today's dispatcher, moved behind the interface.
- Others later (Forgejo, Buildkite...), without touching the core.
- Tests: an in-process fake source, as `fleeting-plugin-dummy` is for
  fleeting.

What the boundary also gives:

- The core restarts a source that dies; gitlab-runner never restarts a
  dead plugin, which is why `fleetci watchdog` exists.
- In the federation, Capacity stays the host's own: a source takes only
  jobs its host has room for, and the ranking of step 2 decides which
  host. The source doesn't change.
- The plugin direction stays: gitlab-runner loading `fleeting-plugin-fleetci`
  is unchanged and needs no source.

Order: the interface in-process first (`internal/source`, GitHub and GitLab
behind it in one binary; done), then go-plugin and separate binaries once
it holds. Plugin mode's GitHub dispatcher uses the same Capacity.

### Costs of the embedded runner

- gitlab-runner's module path has no `/v19`, so it is pinned by commit
  (`v1.11.1-0.20261028153507-66ad5002d924` is v19.5.2) and promises no Go
  API. fleetci follows its releases by hand.
- About 250 more modules (Docker, Kubernetes client, cloud SDKs) and a
  binary of 101 MB against 23 MB, most of it the cloud SDKs of the cache
  adapters and secret resolvers; build tags could leave out the unused ones.

## Step 2: the federation

Replaced by [ha.md](ha.md): machines at several sites, each with its own
budget, behind NAT on one VPN, gain little from consensus and lose a site
that is cut off, which Raft stops as the minority. They share the GitHub
queue by gossip instead, each ranking the machines with room the same way,
and a wrong view costs a runner too many, never a job. A cloud class shared
by machines keeps the lease with a fencing token, after client-go
`leaderelection`, as an option.

What this section planned and the federation leaves out: running a taken
job on another host's VM, and nominations between hosts. A host takes only
jobs it has room for, so neither is needed.

## Step 3: tests of several machines in one process

N members in one test, on memberlist's in-memory transport wrapped in one
that drops, delays and partitions messages. Faults come from a seeded
schedule, so a failure replays exactly. After each run:

- no host ran past its budget;
- no cloud class had two owners at once (fencing tokens only grow);
- no job waited while a member had room for it past the grace periods;
- every job taken started, or failed with its reason.

## Step 4: the demo

Three to five Cloud Hypervisor microVMs from the local kit act as servers,
each running fleetci with a dockervm class, or a fake plugin when testing
scale. headscale (self-hosted Tailscale control, BSD-3) joins them, so the
kit needs no tailnet account; real Tailscale between real machines comes
after.

## Step 5: chaos

- kill -9 a member; stop one with SIGSTOP, which is worse than a crash;
- partitions with nftables, latency and loss with netem;
- clock jumps; a full disk under `state_dir`;
- Tailscale falling back to its DERP relays; expired node keys;
- rolling upgrades.

The invariants of step 3 are checked against the event history of each
run, and the optional lease with Porcupine (MIT) for linearizability.
