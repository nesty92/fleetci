# fleetci across restarts and machines

Status: phase 0 built, but for its GitLab drain and fleetingd adoption.
The federation is planned. It answers #23 and replaces
[multi-host.md](multi-host.md)'s step 2, the Raft cluster.

## The setup this is for

Several machines, at several sites, each with its own CPUs and memory, and
perhaps a cloud class for spikes. They reach each other over one VPN
(WireGuard, Tailscale, headscale), since most sit behind NAT or on private
networks. Each machine's VMs run on that machine and die with it.

So the high availability that matters is:
- **A restart of one machine's fleetci keeps its jobs** (phase 0).
- **The machines share the job queue,** and a machine that goes, or a site
  that is cut off, takes away only its own capacity. The others, and the
  cut-off site itself, keep serving what they can.

A standby on another machine can't save a machine's on-host VMs, and
consensus would do harm here: Raft stops the minority side of a cut, so a
site whose VPN link drops would stop taking jobs it has the machines for.

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
| memberlist (MPL-2.0, imported as a library) | SWIM membership: failure detection by indirect probes, gossip of state, push-pull sync, AES-GCM encryption with a rotatable keyring | Membership and the gossip of each machine's room, over the VPN |
| Serf and Consul (MPL-2.0, ideas) | Events and queries on top of memberlist; members that leave versus fail; reaping failed members after a timeout | Telling a machine that left from one that failed; reaping |
| Rendezvous hashing (Thaler and Ravishankar, 1996) | Every node ranks the same candidates the same way from a hash, with no coordination, and only the keys of a node that goes move | Who polls each repo, and the order in which machines take a job |
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

Several `fleetci serve` processes, one per machine, that know of each other
and share the GitHub queue. Nothing in it needs agreement: each machine
decides for itself from what it has heard, the same way the others do, and
a machine that hears wrong costs a runner too many, never a job.

#### What needs sharing

| | Shared how |
|---|---|
| On-host VMs (fleetingd) | Not at all: each machine's budget is its own |
| GitLab jobs | Not at all: GitLab gives each job to one runner, and each machine asks only while it has room. One `serve` per machine, with its own runners, or the same runner token on each, works today |
| Which machine starts a runner for a GitHub job | Each machine ranks the machines with room the same way (below) |
| Polling GitHub | Each repo is polled by one live machine, chosen by rendezvous hashing; what it finds is gossiped |
| Cloud classes | Not at all: each site has its own fleeting group and `max_instances`. A class shared by machines needs one owner; see the optional lease |

#### Membership

- **memberlist over the VPN,** with its WAN settings (probes every 5 s,
  3 s timeouts), since sites are tens of milliseconds apart. It needs one
  TCP and UDP port.
- **Config:**

  ```toml
  [federation]
  id = "wroclaw-1"                         # default: the hostname
  listen = "100.64.0.11:9481"              # an address on the VPN
  join = ["100.64.0.12:9481"]              # any member or two; more are found
  key_file = "/etc/fleetci/federation.key" # 32 random bytes, base64
  ```

  A new machine joins with one known address and the key: no list of every
  peer, no certificates, no minimum count. One machine alone works as
  today. `key_file` may hold several keys, the first used to send, so the
  key can be rotated without stopping the federation, as memberlist's
  keyring allows.
- **What is gossiped:** each member's id, fleetci version, the repos and
  orgs it serves, and per class its free slots and whether it is on the
  host. Then, as events: jobs found queued, and runners started for them.
  No tokens, no JIT configs, no SSH details: those never leave the machine.
  memberlist's node metadata holds 512 bytes, too few for many classes, so
  the room travels in its broadcasts and push-pull state instead.
- **Left versus failed.** A member that stops says so and is dropped at
  once; one that fails is marked suspect and then dead by SWIM, and reaped
  after an hour.
- **Trust.** Everyone with the key is a member, and a member can claim room
  it doesn't have: the key is a secret like the GitHub token. The VPN is
  the first fence; the key's encryption is the second.

#### Dispatching a GitHub job

For a queued job of class `c` in repo `r`, every machine computes the same
list: the live members that serve `r` and have a free slot of `c`, ranked
by the hash of the job's id with each member's id (rendezvous), the free
room weighing in so that fuller machines rank lower.

- **Rank 0 starts a runner at once** and gossips `started job <id>`.
- **Rank k waits k × `federation.grace`** (10 s by default), then starts one
  if it hasn't heard a start and the job is still queued, which it checks
  with GitHub first.
- **A machine that finds no room for the job** waits, as today, and ranks
  again when the room changes.
- **Views differ for a few seconds** after a member joins, fails or fills
  up. Two machines may then both take rank 0 and both start a runner: one
  takes the job, and the idle watch removes the other, as it already does
  for a runner GitHub gives another job. That is the price of no
  consensus, and it is bounded by the grace period.
- **A cut-off site** sees only its own members, so it serves every job it
  can, and the other side does the same. The price is again runners too
  many, never a job that waits while some machine has room.

#### Polling and webhooks

- **Each repo and org is polled by one live member** that serves it,
  chosen by rendezvous hashing, so N machines use the rate limit once.
  When that member goes, the repo moves to the next in rank within one
  failure detection. Jobs found are gossiped to the members that serve the
  repo.
- **Every member still polls every repo slowly** (every 5 minutes, as with
  a webhook today), so a lost broadcast, or a poller that is alive but
  stuck, costs minutes, not the job. The poller itself polls at
  `poll_interval` (10 s by default).
- **Webhooks** reach whichever member has a public address, or a relay in
  front of one; it gossips the jobs as a poll would. Machines behind NAT
  need none.

#### Optional: a cloud class shared by machines

Only for setups that want one cloud class with one `max_instances` across
machines. Its owner holds a lease, `class/<name>`, with client-go's
timings, on a store all members reach: S3 with conditional writes, or a
shared directory. The owner runs its taskscaler, and every `Increase`,
`Decrease` and `Update` first checks that the lease was renewed within the
renew deadline, so an owner stopped with SIGSTOP refuses to scale when it
resumes. A new owner adopts the VMs as in phase 0, with taskscaler's state
in the same store. Other members don't use the class: a job of it goes to
the owner by the ranking. Without the lease, give each site its own class.

#### Steps

1. **Membership and room.** memberlist, `[federation]`, gossip of room, and
   `fleetci federation status`: the members, their versions and their free
   room, as this machine sees them.
2. **GitHub dispatch by rank,** with the `started` events and the grace.
3. **Polling split,** the slow safety poll, and webhook jobs gossiped.
4. **The rest of phase 0,** alongside: the GitLab drain, then fleetingd
   adoption.
5. **The shared cloud class lease,** if a setup asks for it.

## Tests

- **In one process:** N members on memberlist's in-memory transport,
  wrapped in one that drops, delays and partitions messages from a seeded
  schedule (`internal/chaos`), each with a fake GitHub and fake plugins.
  After each run:
  - no job stayed queued past N × grace while a member had room for it;
  - runners per job: one when the views agree; never more than the members
    that had room;
  - no member ran past its budget;
  - every repo had a poller whenever it had a live member.
- **The demo:** three to five Cloud Hypervisor microVMs from the local kit,
  each a "site" running fleetci with a dockervm class, joined over
  headscale (BSD-3), so the kit needs no tailnet account.
- **Chaos:** kill -9 and SIGSTOP of members; partitions with nftables,
  latency and loss with netem; a site cut off for minutes; Tailscale
  falling back to its DERP relays; rolling upgrades with two versions
  side by side.

## To decide

1. **`grace`:** 10 s is a VM boot on the slowest class. Shorter means more
   duplicate runners when views differ; longer means a slower takeover.
2. **Members with different `[github]` tables:** allowed, ranking only among
   members that serve the repo, or required to match.
3. **Version skew:** members ignore events they don't know, and the status
   shows versions; a minimum version for joining is possible but not
   planned.
4. **fleetingd adoption:** in the next round, or left, with on-host VMs
   lost on a restart for now.
