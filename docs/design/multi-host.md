# Standalone fleetci on many hosts

Status: plan. The first step, the embedded GitLab runner, is a spike on
`feat/standalone-gitlab`.

## Where this goes

fleetci becomes one program that runs anywhere and takes jobs from GitLab
and GitHub itself, with no gitlab-runner beside it. Several machines,
joined over Tailscale, run it together: they share the work, and the
cluster keeps going when one of them fails.

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
- `Prepare` creates the VM from the reservation, and the environment dials
  it with `connector.Dial`.
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

### One runner per class

A job request can't filter by tag, so a runner can only be kept from a
class's jobs by not asking for them. fleetci registers one runner per
class, tagged with the class's name, and asks for a class's job only when
the class has room. Jobs it can't run yet stay pending in GitLab, where
another host may take them. The template moves from the `fleetci` tag plus
`FLEETCI_CLASS` to the class's tag; the current runner stays as a catch-all
so existing pipelines keep working.

What this gives:

1. No job waits inside fleetci, so no `acquire_timeout` failures on local
   classes.
2. Several hosts poll the same class runners and GitLab hands each job to
   one of them: load balancing between hosts with no coordination.
3. One process serves GitLab and GitHub from one host budget.

What it costs:

- gitlab-runner's module path has no `/v19`, so it is pinned by commit
  (`v1.11.1-0.20261028153507-66ad5002d924` is v19.5.2) and promises no Go
  API. fleetci follows its releases by hand.
- About 250 more modules (Docker, Kubernetes client, cloud SDKs) and a
  binary of 101 MB against 23 MB, most of it the cloud SDKs of the cache
  adapters and secret resolvers; build tags could leave out the unused ones.
- One runner registration per class, made by the user or through the API.

Open: GitLab tells a runner nothing about pending jobs before it takes one,
so fleetci can't see that a large job has waited long. Fairness between
classes becomes which class's runner fleetci asks first; how a nomination
works when the job is still in GitLab is decided with the cluster.

## Step 2: the cluster

### What stays local

Each host's budget is the host's own and needs no agreement: a host knows
what runs on it. When the cluster loses quorum, every host keeps running
its jobs and keeps taking jobs of its local classes within its budget. Only
the shared things below pause.

### What needs agreement

| State | Why | How |
|---|---|---|
| Owner of each cloud class | Two processes driving one fleeting group remove each other's VMs | A lease with a fencing token, after Kubernetes client-go `leaderelection` |
| GitHub dispatch | One host creates the just-in-time runner for a job | A claim per job id in the Raft log |
| Nominations between hosts | A long-waiting large job isn't starved by small ones elsewhere | Run by the leader, kept in Raft |
| Hosts, their budgets and free room | Placement and nominations | Gossip (memberlist), heartbeats in Raft |

### Consensus

`hashicorp/raft`, which Nomad, Consul and Vault run on, is MPL-2.0 and is
imported as a library, with `raft-boltdb` for its log, `memberlist` for
finding peers over Tailscale and `raft-autopilot` for removing dead
servers. etcd's `raft` (Apache-2.0) is the alternative; it leaves storage
and transport to us.

Surviving one server's failure takes three voters. With two machines, the
third voter can be anything small: a cloud nano instance, a Raspberry Pi, a
laptop. Further machines join as clients that run jobs and don't vote, as
Nomad's do.

### max_instances

Proposed: per host for classes on the host, so each host's own limit holds;
for the whole cluster for cloud classes, which have one owner. To decide
before the cluster is built.

### Where the ideas come from

- Kubernetes (Apache-2.0, may be ported with credit): leader election with
  leases; the node lifecycle controller, which marks a node NotReady after
  a grace period.
- Kueue (Apache-2.0): cohorts that borrow quota, for cloud overflow shared
  by hosts.
- Nomad (BUSL, ideas only, no code): servers and clients; heartbeat TTLs
  that grow with the cluster; plan and apply with optimistic concurrency;
  what happens to jobs on a client that disconnects.
- Slurm (GPL, ideas only): down and drain states for nodes.

## Step 3: tests of several machines in one process

A cluster of N nodes in one test, on Raft's in-memory transport wrapped in
one that drops, delays and partitions messages. Faults come from a seeded
schedule, so a failure replays exactly. After each run:

- no host ran past its budget;
- no cloud class had two owners at once (fencing tokens only grow);
- no job was claimed twice;
- every job taken started, or failed with its reason.

## Step 4: the demo

Three to five Cloud Hypervisor microVMs from the local kit act as servers,
each running fleetci with a dockervm class, or a fake plugin when testing
scale. headscale (self-hosted Tailscale control, BSD-3) joins them, so the
kit needs no tailnet account; real Tailscale between real machines comes
after.

## Step 5: chaos

- kill -9 the leader; stop a server with SIGSTOP, which is worse than a
  crash;
- partitions with nftables, latency and loss with netem;
- clock jumps; a full disk under Raft's directory;
- Tailscale falling back to its DERP relays; expired node keys;
- rolling upgrades.

The invariants of step 3 are checked against the event history of each
run, and the claim and lease API with Porcupine (MIT) for linearizability.
