# fleetci that outlives its process

Status: plan. It answers #23 and replaces the order of
[multi-host.md](multi-host.md)'s step 2: high availability first, on a
store fleetci already has, and the Raft cluster later, as one more backend
of the same store.

## Where things stand

A fleetci process that stops takes its jobs with it, and a second process
can't stand by:

1. **The GitHub runner dies with fleetci.** `SSHStart` runs it inside
   fleetci's SSH session (`internal/github/runner.go`), so when the process
   goes, the session closes and the runner gets SIGHUP, however healthy the
   VM is.
2. **A new process removes every VM it finds.** `dockerReady` rejects an
   instance whose cause is `preexisted` (`internal/fleet/backend.go`): it
   may have run a job already, and nothing says whether it did.
3. **Stopping removes every VM.** `scalerOptions` sets
   `WithDeleteInstancesOnShutdown`, running jobs included.
4. **On-host VMs die with their plugin.** fleetingd starts Cloud Hypervisor
   with `Pdeathsig: SIGKILL` and keeps its inventory in memory, so even a
   plugin restart by `supervise` loses them.
5. **Two processes can't share a cloud class.** Each would remove the
   other's instances as unknown; `startPool` already refuses two classes on
   one group in one process, and nothing stops it across processes.

Everything the process knows that the cloud doesn't, it keeps in memory:
which instance runs which job, use counts, the dispatcher's `taken` jobs.

## What the reference projects do

| Project | What it does | What fleetci takes |
|---|---|---|
| taskscaler (MIT) | `WithStorage` saves each instance's slots, acquisition keys and use count; on start, `createInstanceOrRestore` restores them and the UpFunc sees `Restored` and the keys | The handover itself: a store the next process reads, through taskscaler's own `storage.Storage` interface |
| gitlab-runner (MIT) | `state_storage` uses it, but releases every restored acquisition and removes the instance, since its jobs ran in the dead process | Only VMs whose job lives on the VM can be kept; embedded GitLab jobs can't, so they drain |
| Karpenter (Apache-2.0) | The cloud is the truth: the GC controller lists instances, deletes those no NodeClaim claims after 30 s, every 10 s at first and every 2 min later | A sweep for instances nobody's state claims, with a grace period, in place of "remove all preexisting" |
| client-go `leaderelection` (Apache-2.0) | Lease of 15 s, renewed within 10 s, retried every 2 s; expiry is measured from when the observer last saw the record change, so clocks needn't agree; a leader that can't renew in time stops itself | The lease and its timings, ported with credit |
| terraform-aws-github-runner (MIT) | Stateless control plane; scale-down asks GitHub whether a runner is busy, and found the flag stale for 25–60 s and even 12+ min, so it needs readings over a window | Never decide from GitHub's `busy` flag; ask the VM |
| actions/scaleset (MIT) | One message session per scale set; a second listener gets 409 Conflict | Not used: fleetci's JIT runners have no session. If it moves to scale sets, the session is a free lock for dispatch |
| Nomad (BUSL, ideas only) | A client that loses its servers keeps running its allocations for a grace period; heartbeat TTLs grow with the cluster | A host cut off from the store keeps its running jobs; only shared things pause |

## The plan

### Phase 0: a restart keeps running jobs

One process, no lease. An upgrade or a crash of `fleetci serve` no longer
fails the jobs on a cloud class.

- **The runner on the VM, detached.** `SSHStart` writes the JIT config to a
  file of mode 0600 and starts the run script under `setsid`, with its
  output to `~/.fleetci/runner.log` and its exit status to
  `~/.fleetci/runner.exit`. It doesn't need systemd, so it works on any
  image. fleetci follows the job on a session of its own (`tail -F` of the
  log, then the exit file), and losing that session no longer touches the
  runner. The log lines it parses today, such as the job taken, stay the
  same.
- **taskscaler state on disk.** `WithStorage(storage.NewFileStorage(dir))`
  per class, in a `state_dir` of fleetci.toml.
- **Adoption in the UpFunc.**
  - **Restored, with acquisitions:** a job ran on it. fleetci reconnects and
    follows the runner: still running, it waits for the exit and then
    releases the key, so the VM goes as after any job. Exited while fleetci
    was down, it releases at once. Unreachable, it's removed.
  - **Restored, no acquisitions:** a warm VM that never ran a job, since
    the use count was saved. It's kept.
  - **Preexisted, not restored:** nothing says what it ran; it's removed, as
    today.
- **Stopping drains.** SIGTERM stops taking jobs, removes idle VMs, and
  leaves VMs with jobs to the next process. `WithDeleteInstancesOnShutdown`
  goes. `fleetci serve --remove-all` keeps today's behaviour, for tearing a
  setup down.
- **GitLab.** Embedded GitLab jobs run their executor in fleetci's process
  and can't be handed over. SIGTERM stops asking for jobs and waits up to
  `drain_timeout` for the running ones, as gitlab-runner does on SIGQUIT.
  Plugin mode doesn't change: gitlab-runner owns that process.
- **On-host classes (fleetingd), optional, a change to the plugin.** Drop
  `Pdeathsig`, keep the inventory in its state directory, and adopt running
  Cloud Hypervisor processes on start. Without it, a restart still loses
  on-host VMs, and the plan doesn't depend on it.

The acceptance:
- **kill -9 mid-job:** `kill -9` of `serve` during a long GitHub job on an
  AWS class, then a restart: the job finishes green, and its VM is removed
  after it.
- **Warm VM kept:** an idle warm VM survives the restart and takes the
  next job.

### Phase 1: a standby (#23)

Two processes, one active.

- **A store** with two primitives, a lease and taskscaler's instance state:

  ```go
  type Store interface {
  	// Lease takes or renews name for holder. Token grows each time the
  	// holder changes.
  	Lease(ctx context.Context, name, holder string, ttl time.Duration) (Lease, error)
  	Release(ctx context.Context, l Lease) error
  	// Claim records key for holder unless a live claim exists (phase 2).
  	Claim(ctx context.Context, key, holder string, ttl time.Duration) (bool, error)
  	// Instance state of one class, as taskscaler's storage.Storage.
  	Instances(class string) storage.Storage
  }
  ```

  Backends:
  - `dir`: flock and files. Two processes on one machine, and the tests.
  - `s3`: conditional writes (`If-None-Match: *` to take a lease, `If-Match`
    on its ETag to renew it). It works on any S3 that has them, such as the
    bucket a setup already has for its CI cache, and needs no new service.
    fleetci has no S3 client of its own yet; this adds one.
  - Raft later (phase 3); the interface doesn't change.
- **One lease, `active`,** with client-go's timings: 15 s lease, renewed
  within 10 s, tried every 2 s. Expiry is counted from when the standby last
  saw the record change, so clocks needn't agree.
- **Takeover** is phase 0's restart, reading the shared instance state.
  Before it places a job on an adopted idle VM, the new active waits out
  one lease period, so a delete the old one had in flight can't hit a VM
  that has just been given a job.
- **Fencing.** The cloud's API takes no token, so the old active fences
  itself. The group taskscaler drives is wrapped (`initialised` already
  wraps it), and every `Increase`, `Decrease` and `Update` first checks that
  the lease was renewed within the renew deadline; otherwise it fails
  without calling the plugin. After a SIGSTOP the check sees the stopped
  time and refuses. A process that can't renew stops dispatching, stops its
  taskscalers without removing anything, and goes back to standby.
- **Webhooks.** Both processes can sit behind one URL; the standby answers
  503. A takeover's first poll lists every repo whole, so a delivery lost in
  the gap costs a poll interval, not a job.

The acceptance, #23's own:
- **kill -9 of the active:** the standby takes jobs within a lease period,
  and the jobs running on the old active's VMs finish.
- **No VM with a job is removed** during the handover.
- **One process alone** works as in phase 0.

### Phase 2: both active

Several processes, all taking jobs. This is what multi-host.md step 2
needs, minus placing VMs on other hosts.

- **On-host classes are always on.** A host's budget is its own, and
  nothing about it needs agreement.
- **Each cloud class has its own lease,** `class/<name>`; any process may
  hold it, and only the holder runs its taskscaler. With one owner,
  `max_instances` is global by construction, as multi-host.md proposed.
- **GitHub: one claim per job id** (`Claim("job/<id>", ttl)`) before a
  runner starts, taken only by a process with room for the job's class.
  Hosts with room take jobs and full hosts leave them, which answers
  multi-host.md's "a full host takes a job another host has room for"
  without moving VMs between hosts. The existing check for jobs taken
  elsewhere stays, for runners GitHub gives another job anyway.
- **GitLab is safe already:** GitLab gives each job to one runner, and the
  embedded runners ask only while they have room.

### Phase 3: the Raft cluster

multi-host.md's step 2 and later, unchanged in substance:
- running a taken job on another host's VM;
- nominations between hosts;
- gossip of free room.

Raft becomes the store's third backend; leases and claims keep their API,
and phases 1 and 2 don't change. Steps 3–5 there (the in-process cluster
tests, the demo, chaos) apply to every backend.

## Tests

- Each phase:
  - **kill -9:** of the active mid-job.
  - **SIGSTOP:** of the active for longer than the lease, then SIGCONT; it
    must refuse to scale, the case client-go's design is for.
  - **Partition:** the active cut off from the store but not from the
    cloud, which is the dangerous one.
  - **Lost VM:** the VM unreachable during adoption.
  - **Restart loops:** restarts every few seconds under load.
- **Invariants after each run,** from the event history:
  - no VM with a running job removed;
  - no job started twice;
  - no two holders of a lease at once (tokens only grow);
  - no host over its budget.
- **Where they run:** in-process on the `dir` store and a fake plugin, in
  `internal/chaos` with the existing fault schedules; then against real
  AWS and S3, as the reclaim tests did.

## To decide

1. **Order:** this ahead of the Raft cluster, Raft becoming a store backend.
   Recommended: it delivers #23 and most of multi-host.md's problem 3
   without consensus.
2. **First backend:** `dir` and `s3`. DynamoDB is the alternative, but
   it's one more service to set up for the same thing.
3. **SIGTERM:** it starts draining instead of removing everything, and
   `--remove-all` does what it did. This changes `serve`'s behaviour, not
   the plugin's.
4. **fleetingd adoption:** do it in phase 0, or leave on-host VMs lost on a
   restart for now.
