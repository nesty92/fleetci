# fleetci across restarts and machines

Status: phase 0 built, but for its GitLab drain and fleetingd adoption.
The federation is planned. It answers #23 and replaces
[multi-host.md](multi-host.md)'s step 2, the Raft cluster.

## The setup this is for

Several machines, at several sites, each with its own CPUs and memory, and
perhaps a cloud class for spikes. Many sit behind NAT or on private
networks, so they may not see each other at all; all of them reach GitHub
(or GitLab). Each machine's VMs run on that machine and die with it.

So the high availability that matters is:
- **A restart of one machine's fleetci keeps its jobs** (phase 0).
- **The machines share the job queue,** and a machine that goes, or a site
  that is cut off, takes away only its own capacity. The others, and the
  cut-off site itself, keep serving what they can.

A standby on another machine can't save a machine's on-host VMs, and
consensus would do harm here: Raft needs the machines to reach each other,
and stops the minority side of a cut, so a site whose link drops would stop
taking jobs it has the machines for.

## Where things stood

A fleetci process that stopped took its jobs with it, and two processes
couldn't share work:

1. **The GitHub runner died with fleetci.** `SSHStart` ran it inside
   fleetci's SSH session, so when the process went, the runner went too,
   however healthy the VM was. *Fixed in phase 0.*
2. **A new process removed every VM it found.** `dockerReady` rejects an
   instance whose cause is `preexisted`: nothing said whether it had run a
   job. *Fixed in phase 0 for classes with `state_dir`.*
3. **Stopping removed every VM,** running jobs included. *Fixed in phase 0.*
4. **On-host VMs die with their plugin.** fleetingd starts Cloud Hypervisor
   with `Pdeathsig: SIGKILL` and keeps its inventory in memory.
5. **Two processes can't serve the same GitHub repos.** Each starts a
   runner for every queued job, so all but one runner per job sit idle
   until the dispatcher's idle watch removes them, and each polls every
   repo, using the token's rate limit once per machine.
6. **Two processes can't share a cloud class.** Each would remove the
   other's instances as unknown.

## What the reference projects do

| Project | What it does | What fleetci takes |
|---|---|---|
| taskscaler (MIT) | `WithStorage` saves each instance's slots, acquisition keys and use count; on start, `createInstanceOrRestore` restores them | Phase 0's handover across a restart |
| gitlab-runner (MIT) | `state_storage` releases every restored acquisition, since its jobs ran in the dead process | Only jobs that live on the VM can be kept; embedded GitLab jobs drain instead |
| GitHub's runner API | A runner's name is unique within its repo or org; a second registration of it gets 409 Conflict | The claim on a job, with no network between machines |
| memberlist (MPL-2.0, imported as a library) | SWIM membership: failure detection by indirect probes, gossip of state, push-pull sync, AES-GCM encryption with a rotatable keyring | Optional membership, where the machines see each other |
| Serf and Consul (MPL-2.0, ideas) | Events and queries on top of memberlist; members that leave versus fail; reaping failed members after a timeout | Telling a machine that left from one that failed; reaping |
| Rendezvous hashing (Thaler and Ravishankar, 1996) | Every node ranks the same candidates the same way from a hash, with no coordination, and only the keys of a node that goes move | Who polls each repo, with membership |
| terraform-aws-github-runner (MIT) | Found GitHub's `busy` flag stale for 25–60 s and even 12+ min | Decide from what the VM and the job say, not `busy` |
| client-go `leaderelection` (Apache-2.0) | Lease of 15 s, renewed within 10 s, retried every 2 s, expiry measured by the observer | Only for the optional shared cloud class (below) |

## The plan

### Phase 0: a restart keeps running jobs

One process, no lease. An upgrade or a crash of `fleetci serve` no longer
fails the jobs on a cloud class.

- **The runner on the VM, detached** (`internal/vmjob`). `SSHStart` writes
  the run script and the JIT config into `~/.fleetci/jobs/<vm>/`, mode
  0700, and starts the script under `setsid`, with its output to `log`
  there, its process group to `pid` and its exit status to `exit`. The
  config file is removed as soon as the script has opened it. It doesn't
  need systemd, so it works on any image. fleetci follows the job on a
  session of its own, polling the log and then the exit file, and losing
  that session, or the connection, no longer touches the runner: fleetci
  dials again and goes on from the line it had read. The log lines it
  parses, such as the job taken, stay the same.
- **taskscaler state on disk.** `WithStorage(storage.NewFileStorage(dir))`
  per class not on this host, in `<state_dir>/classes/<class>`, with
  `state_dir` in fleetci.toml (`serve` only; the plugin refuses it).
- **Adoption in the UpFunc.**
  - **Restored, with acquisitions:** a job ran on it. fleetci reconnects and
    follows the runner: still running, it waits for the exit and then
    releases the key, so the VM goes as after any job. Exited while fleetci
    was down, or with no job there (a GitLab job's VM), it releases at
    once. Unreachable for 10 minutes, it's removed.
  - **Restored, no acquisitions:** a warm VM that never ran a job, since
    the use count was saved. It's kept.
  - **Preexisted, not restored:** nothing says what it ran; it's removed, as
    today.
- **Stopping hands over.** SIGTERM stops taking jobs. The dispatcher hands
  each VM whose runner took a job to the next process (`Capacity.HandOver`)
  instead of removing it and its runner; a runner that took no job yet goes
  with its VM, and its job waits. `Backend.Stop` then removes every other
  VM, idle ones too, and leaves the handed-over and adopted ones.
  `WithDeleteInstancesOnShutdown` stays for classes without stored state.
  `fleetci serve -remove-all`, or a config without `state_dir`, keeps
  today's behaviour, for tearing a setup down.
- **GitLab (not yet).** Embedded GitLab jobs run their executor in
  fleetci's process and can't be handed over; today they end with it, and
  their VMs are removed. To do: SIGTERM stops asking for jobs and waits up
  to `drain_timeout` for the running ones, as gitlab-runner does on
  SIGQUIT. Plugin mode doesn't change: gitlab-runner owns that process.
- **On-host classes (fleetingd), not yet, a change to the plugin.** Drop
  `Pdeathsig`, keep the inventory in its state directory, and adopt running
  Cloud Hypervisor processes on start. Without it, a restart still loses
  on-host VMs, which is why their classes keep no state, and the plan
  doesn't depend on it.

The acceptance:
- **kill -9 mid-job:** `kill -9` of `serve` during a long GitHub job on an
  AWS class, then a restart: the job finishes green, and its VM is removed
  after it.
- **Warm VM kept:** an idle warm VM survives the restart and takes the
  next job.

### The federation

Several `fleetci serve` processes, one per machine, sharing the GitHub
queue. They need no network between them: GitHub is the one thing every
machine already reaches, and it settles who runs a job. A network between
the machines, when there is one, only saves rate limit and shows status.

#### What needs sharing

| | Shared how |
|---|---|
| On-host VMs (fleetingd) | Not at all: each machine's budget is its own |
| GitLab jobs | Not at all: GitLab gives each job to one runner, and each machine asks only while it has room. One `serve` per machine, with its own runners or the same runner token, works today |
| Which machine starts a runner for a GitHub job | GitHub: the runner's name is the claim (below) |
| Polling GitHub | Each machine polls alone; with membership, each repo is polled by one machine |
| Cloud classes | Not at all: each site has its own fleeting group and `max_instances`. A class shared by machines needs one owner; see the optional lease |

#### The claim is the runner's name

GitHub keeps a runner's name unique within its scope, a repo or an org,
and refuses a second registration of a name with 409 Conflict. So a
runner started for a job is named after the job:

```
fleetci-<class>-j<job id>-<try>
```

- **Registering is claiming.** A machine registers the runner first, and
  takes a VM only once GitHub said yes. Another machine that tries the same
  name gets 409, gives back the room it had reserved, and leaves the job.
  Reserving room is instant, so this costs one GitHub call, about half a
  second, before the VM is taken; today the registration runs alongside
  taking the VM.
- **Machines with room try first.** A machine waits `grace × used ÷ slots`
  of the job's class, plus a little jitter, before it tries: an empty
  machine at once, a fuller one later, a full one not at all. Without
  knowing of each other, the machines with the most room win. `grace`
  defaults to 2 s.
- **The claim lasts as long as the runner.** The machine that holds it
  unregisters the runner when the runner fails, and starts the next try
  itself under `<try>+1`, as it does today. One whose runner took another
  job, which GitHub may do, knows from the runner's log and retries the
  job the same way.
- **A machine that dies holding a claim** leaves a registered runner
  nobody will start. The others see the job still queued after
  `claim_timeout` (3 minutes, beyond the slowest boot) and try the next
  `<try>`.
- **Before registering,** a machine checks the job is still queued when
  its list is older than a few seconds, as `takenElsewhere` does, so a
  name GitHub freed when the job ended isn't claimed again.
- **Every machine must use the same scope** for a repo: all of them list it
  under `repos`, or all under `orgs`. Names are unique per scope, so a repo
  runner and an org runner of the same name don't collide. `fleetci check`
  can't see the other machines' configs; the runbook says so.

Two machines never start runners for the same job, whatever the network
does, because GitHub decides. A cut-off site serves the jobs it can, as
long as it reaches GitHub.

#### Membership, optional

Without it, each machine polls every repo it serves, so N machines use the
token's rate limit N times. A GitHub App gets 5,000 requests an hour, up
to 12,500 with many repos, and an idle poll costs about a call per repo,
so a few machines with tens of repos at the default `poll_interval` run
out. Either raise `poll_interval` (a webhook makes that free), or let the
machines know of each other:

- **Gossip, where the machines see each other** over any network: a LAN,
  a VPN, public addresses. memberlist (SWIM) with its WAN settings, one
  TCP and UDP port, encrypted with a shared key:

  ```toml
  [federation]
  id = "wroclaw-1"                         # default: the hostname
  listen = "10.1.0.11:9481"
  join = ["10.2.0.12:9481"]                # any member or two; more are found
  key_file = "/etc/fleetci/federation.key" # 32 random bytes, base64
  ```

  memberlist expects every member to reach every other one. A member that
  reaches only the one it joined is soon taken for dead by the rest.
- **Through a shared store, where they don't:** each machine writes a
  heartbeat object to S3 (or a shared directory) every 10 s, and reads
  the others'. Only the store needs to be reachable, from anywhere.
- **What membership gives:** each repo is polled by one live member, chosen
  by rendezvous hashing, with the jobs it finds passed to the others
  (gossip, or a queue object in the store). Every member still polls every
  repo every 5 minutes, so a lost message or a stuck poller costs minutes,
  not the job. `fleetci federation status` shows the members, their
  versions and their room.
- **Webhooks** reach whichever member has a public address, or a relay;
  with membership, it passes the jobs on as the poller does.

Membership is never needed for correctness: with none, or a broken one,
each machine falls back to polling alone.

#### Optional: a cloud class shared by machines

Only for setups that want one cloud class with one `max_instances` across
machines. Its owner holds a lease, `class/<name>`, with client-go's
timings, in a store all members reach: S3 with conditional writes, or a
shared directory. The owner runs its taskscaler, and every `Increase`,
`Decrease` and `Update` first checks that the lease was renewed within the
renew deadline, so an owner stopped with SIGSTOP refuses to scale when it
resumes. A new owner adopts the VMs as in phase 0, with taskscaler's state
in the same store. Other members don't serve the class. Without the lease,
give each site its own class.

#### Steps

1. **Verify GitHub's 409** on a real repo and an org: the same name twice,
   at the same moment from two clients, and again after the first runner
   ended.
2. **Claims by name,** with the room-weighted wait and `claim_timeout`. This
   alone makes several machines on the same repos safe.
3. **Membership through the store, then gossip,** with the polling split
   and `fleetci federation status`.
4. **The rest of phase 0,** alongside: the GitLab drain, then fleetingd
   adoption.
5. **The shared cloud class lease,** if a setup asks for it.

## Tests

- **In one process:** N dispatchers on one fake GitHub (`forge_test.go`
  already keeps names unique), each with fake plugins, under `internal/chaos`
  fault schedules: GitHub slow or erring, machines killed and stopped. After
  each run:
  - no job got runners from two machines at once;
  - no job stayed queued past grace plus `claim_timeout` while a machine
    had room for it;
  - no machine ran past its budget;
  - with membership, every repo had a poller whenever it had a live member.
- **The demo:** three to five Cloud Hypervisor microVMs from the local kit,
  each a "site" running fleetci with a dockervm class, on networks that
  can't see each other, all on the same GitHub repos.
- **Chaos:** kill -9 and SIGSTOP of members; a site cut off from the
  others but not from GitHub, and from GitHub too; rolling upgrades with
  two versions side by side.

## To decide

1. **`grace` and `claim_timeout`:** 2 s and 3 minutes. A shorter grace
   favours an empty machine less; a shorter timeout takes over a dead
   machine's job sooner, at the risk of a second runner for a slow boot.
2. **Which membership first:** the store (works anywhere, needs S3 or a
   shared disk) or gossip (no store, needs the machines to see each other).
3. **Members with different `[github]` tables:** allowed, as long as each
   repo's scope matches.
4. **fleetingd adoption:** in the next round, or left, with on-host VMs
   lost on a restart for now.
