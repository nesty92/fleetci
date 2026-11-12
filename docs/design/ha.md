# fleetci across restarts and machines

Status: phase 0 built, but for its GitLab drain and fleetingd adoption.
Several machines, a leader and its workers, are planned. This answers #23
and replaces [multi-host.md](multi-host.md)'s step 2, the Raft cluster.

## The setup this is for

Several machines, at several sites, each with its own CPUs and memory, and
perhaps a cloud class for spikes. Many sit behind NAT or on private
networks, so they may not see each other; all of them reach GitHub (or
GitLab), and they can reach one or two machines that are reachable, the
controllers. Each machine's VMs run on that machine and die with it.

What the user does is connect fleetci to GitHub or GitLab once, on the
controller. Adding a machine is one command on it. The machines never
register anywhere as runners or apps: they are workers.

So the high availability that matters is:
- **A restart of a machine's fleetci keeps its jobs** (phase 0).
- **A lost worker takes away only its own capacity.** The leader places
  jobs on the others.
- **A lost leader takes away nothing that runs.** Running jobs talk to the
  forge themselves, and a standby takes over finding and placing new ones.

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
6. **Several GitLab machines guess.** Each takes a job before it knows the
   job's class, so a full machine can take a job another had room for, and
   each machine is a registration to keep.
7. **Two processes can't share a cloud class.** Each would remove the
   other's instances as unknown.
8. **A new machine is an afternoon:** KVM, Cloud Hypervisor, fleetingd,
   sysctls, the image and the snapshot, by hand from the runbook, and
   nothing proves the microVMs work until a job fails.

## What the reference projects do

| Project | What it does | What fleetci takes |
|---|---|---|
| taskscaler (MIT) | `WithStorage` saves each instance's slots, acquisition keys and use count; on start, `createInstanceOrRestore` restores them | Phase 0's handover across a restart |
| gitlab-runner (MIT) | `state_storage` releases every restored acquisition, since its jobs ran in the dead process. In `network/gitlab.go`, only `RequestJob` sends the runner token; `UpdateJob`, `PatchTrace` and the artifact calls send the job's own token | A GitLab job taken on one machine can run on another that holds no runner token |
| GitHub's runner API | A runner's name is unique within its repo or org; a second registration of it gets 409 Conflict | Two leaders at once can't start two runners for one job |
| kubeadm (Apache-2.0, ideas) | `kubeadm join` with a short-lived bootstrap token and the CA's hash; the kubelet makes its key and gets its certificate signed (TLS bootstrap) | The join, with nothing copied by hand |
| k3s (Apache-2.0, ideas) | One join string that carries the CA's hash and the secret; the agent dials the servers and keeps the connection, so agents behind NAT work | Workers dial out; one string to join |
| Kubernetes nodes and scheduler (Apache-2.0, ideas) | A node is `Ready` only when its checks pass; cordon and drain; MostAllocated and LeastAllocated scoring | `Ready` after the microVM check; `pack` and `spread` across workers, as fleetci's host queue already does |
| Nomad (BUSL, ideas only, no code) | Clients dial servers and heartbeat on a TTL; a client missing its TTL is marked down, and its work waits for it rather than being run twice | Workers' heartbeats; a lost worker's jobs aren't run again while the forge says they run |
| terraform-aws-github-runner (MIT) | Found GitHub's `busy` flag stale for 25–60 s and even 12+ min | Decide from what the VM and the job say, not `busy` |

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

### Several machines: a leader and its workers

```
                 GitHub / GitLab
                   ▲          ▲  the runner on the VM (GitHub) or the
   finds jobs,     │          │  worker (GitLab) reports the job itself
   creates runners │          │
          ┌────────┴──┐       │
          │ leader    │◄──────┼───── standby (takes over)
          └───▲───▲───┘       │
   watches,   │   │           │
   reports by │   │           │
   the worker │   │           │
         ┌────┴┐ ┌┴────┐      │
         │ wkr │ │ wkr ├──────┘
         └─────┘ └─────┘   each behind its own NAT
```

Two roles, in the same `fleetci` binary; a machine can have both, and a
single machine is both, as `fleetci serve` is today:

- **Controllers** hold the forge credentials: the GitHub App key or token,
  the GitLab runner token. One is the leader; the others are standbys.
  Only the leader talks to the forge to find and take jobs. It has no
  classes of its own, unless it is also a worker.
- **Workers** hold no forge credentials and no class config. Each owns
  its budget and its VMs, runs the classes the controllers give it, and
  tells them its room. A worker can also own a cloud class, with that
  cloud's credentials.

#### The connection: workers dial out, and watch

The fleet's API is HTTPS and JSON, as Kubernetes's is between the kubelet
and the API server: versioned paths under `/v1`, HTTP/2, mutual TLS, one
TCP port (9481). Workers dial every controller; no controller dials a
worker, so workers can be behind any NAT. (Workers on one network also
check which of them reach each other: see Neighbours, below.) Controllers dial each other on the same
port. It works through HTTP proxies, and `curl --cert` can read it.

It is declarative. The leader never calls a worker; it writes
**assignments**, and each worker makes its own so:

- **An assignment** is a VM of a class, and later the job to run on it: a
  GitHub job's JIT config, or a GitLab job as GitLab gave it with the
  `[[runners]]` table it runs under. The leader may also mark it
  cancelled.
- **The worker watches** its assignments on every controller with a long
  poll (`GET /v1/workers/{id}/assignments?rev=…`, answered at the next
  change or after 30 s), boots the VM, runs the job once it is set, and
  removes the VM when the job ends or is cancelled.
- **The worker reports** (`PUT /v1/workers/{id}/status`) to every
  controller when something changes and at least every 5 s: its facts,
  its room per class, its VMs, and each assignment's phase (Pending,
  Ready, Running, Succeeded, Failed). That is also its heartbeat. The
  answer carries its classes and whether it is drained.

A worker decides an assignment itself: it takes room for it or fails it
with `NoRoom`. So a leader working from stale room, or two leaders at
once, can't overbook a worker: the leader tries another. An assignment no
controller lists any more is not cancelled: its controller may have gone,
and the worker sees the job through. A new leader learns what runs where
from the workers' reports.

#### Trust: mutual TLS, from the fleet's own CA

Every connection, worker to controller and controller to controller, is
mutual TLS 1.3, and nothing on port 9481 answers without a client
certificate except the join (below). Both sides check the other's
certificate against the fleet's CA, never the system's.

- **The CA** is made by `fleetci init`: an Ed25519 key in
  `/etc/fleetci/pki/ca.key`, readable by root only, on the controllers
  alone. Workers hold only the CA's certificate.
- **Identity is in the certificate.** A worker's carries `CN=<worker id>`
  and `O=fleetci:worker`; a controller's, `O=fleetci:controller`, plus its
  addresses as SANs, which workers check. The role decides what the
  API allows: only a controller writes assignments or runs fleetci's
  commands, and a worker reaches only its own paths, under the id in its
  certificate.
- **Keys never move.** Each machine makes its own key at join and sends a
  CSR; the controller signs it. The bootstrap token only authorises that
  one signing, as kubeadm's does: it is single-use per machine, expires
  (1 h by default), and is the only thing the join port accepts without a
  certificate.
- **Short certificates, renewed over mTLS.** A worker's certificate
  lasts 30 days, and the worker asks for a new one
  (`POST /v1/workers/{id}/certificate`) after two thirds of that, with a
  new key. A worker off for longer joins
  again with a new token.
- **Revocation is a denylist** the controllers share and check on every
  request: `fleetci leave` and `fleetci revoke <worker>` add the
  certificate's serial, and its next request is refused. Short lifetimes keep the list small; an entry goes when
  its certificate would have expired.
- **The CA rotates** by overlap: `fleetci ca rotate` adds a new CA, the
  controllers trust both and sign with the new one, workers pick up the
  new chain on renewal, and the old CA is dropped once no live
  certificate uses it.

#### Neighbours: which workers reach each other

Workers on one network can later copy caches to each other, directly,
without anything passing through a controller or another network. For
that, each worker first finds which of the others it reaches:

- **Each worker answers its peers** on its own port, `peer_listen` in
  `worker.toml` (`:9483` by default; `"off"` for none). It tells the
  controllers where in its report: `peer_advertise`, or by default every
  address it has but loopback and link-local.
- **The controllers name the peers.** Each status answer lists the other
  workers that report, answer peers and aren't revoked: their ids, their
  certificates' serials and their addresses.
- **Each worker checks them** with mutual TLS, the fleet's certificates
  on both sides. It believes a peer only if it shows a worker
  certificate from the fleet's CA, with the id and the serial the
  controllers named, so a revoked or an old certificate doesn't pass.
  Each worker answers only the peers the controllers named to it.
- **It times them:** the best of three requests on an open connection,
  per address. It checks again when the controllers name others, and
  every 30 s. What it reaches, and how fast, is in its report, and
  `fleetci fleet status` shows it:

```
WORKER  VERSION  STATE  …  NEIGHBOURS  NOTE
w1      v0.8.0   Ready  …  w2 40µs
w2      v0.8.0   Ready  …  w1 40µs
```

A worker that reaches none of the others works as before. Nothing is
copied between workers yet.

#### GitHub

1. The leader finds a queued job, by polling or from the webhook, which
   points at the controllers. Only the leader polls, so the rate limit is
   spent once.
2. It picks a worker with room for the job's class, by the host queue
   fleetci already has: `pack` or `spread` across workers, `nominate_after`
   across the fleet, and the class's `overflow` once no worker has room.
3. It registers the runner first, named after the job:
   `fleetci-<class>-j<job id>-<try>`. GitHub refuses a second registration
   of the name with 409 Conflict, so two leaders at once can't start two
   runners for the job; the one that gets 409 leaves the job.
4. It sends `Run` with the JIT config. The worker boots a VM and starts the
   runner, detached as in phase 0. The runner talks to GitHub itself; the
   worker never sees the App key, and the config is good for one runner.
5. The worker reports the runner taking the job, and its exit. A failed
   runner is retried by the leader under `<try>+1`, on any worker.

#### GitLab

1. The leader alone asks GitLab for jobs, with the one runner token, the
   `fleetci` tag and the class in `FLEETCI_CLASS`, as today. It asks only
   while some worker has room for the largest class, so whatever comes
   fits somewhere.
2. GitLab gives the job, with its class. The leader picks a worker as for
   GitHub, and sends `Run` straight away.
3. The worker runs the build: gitlab-runner's `processBuildOnRunner` with
   the Docker executor on its own VM. State, logs, artifacts and cache go
   from the worker to GitLab with the job's token, which is all those
   calls need; the runner token stays on the controllers.
4. A job's `[[runners]]` table may hold cache credentials. They reach the
   worker with the job, in its assignment, and stay in memory.

The time a job spends on the leader is one placement, milliseconds. A
leader that dies in it leaves a job GitLab thinks is running and nobody
runs; GitLab fails it when it goes stale. That is the one window, and it
needs a crash at that moment.

#### Leader and standbys

The controllers pick the leader with no consensus, where live is a
heartbeat seen in the last 10 s. A leader keeps the lead while it is
live, as a Kubernetes lease holder does, so a controller that comes back
does not take it back and move the jobs in flight twice. With no live
leader, the live controller with the lowest id takes it; of two leaders
after a cut, the lower keeps it. A controller that starts listens to its
peers for a few seconds before it claims anything. That needs no quorum
and no third machine, and a site cut off with a controller elects its own
and keeps working.

A leader that steps down leaves its runners on their workers, even those
that took no job yet: the worker stops one that stays idle, and the next
leader leaves the job to it, by its name, for the claim timeout.

Two leaders at once, in a cut or a slow takeover, do no harm, because
something else already stops each double action:

| Two leaders could… | What stops it |
|---|---|
| take the same GitLab job | GitLab gives each job to one request |
| start two runners for one GitHub job | GitHub refuses the second name (409) |
| overbook a worker | the worker reserves its room itself |
| run a cloud class twice | a cloud class belongs to one worker, not to the leader |

A new leader learns the state from the workers, which each report their
jobs on connect, and from GitHub's runner list, where every `fleetci-…-j…`
runner names its job and try. It then polls as before. Nothing is kept on
the leader that the workers and the forge don't have.

#### Losing a worker

A worker whose status reports stop for 15 s is marked
`Unreachable`. Nothing placed on it is run again yet: its runners and
GitLab builds may still be running and reaching the forge.

- **GitHub:** a job whose runner never came online is tried again under
  `<try>+1` after `claim_timeout` (3 minutes). A job in progress is left to
  GitHub: it finishes, or GitHub fails it when its runner is lost.
- **GitLab:** the job is the worker's; it finishes, or GitLab fails it when
  it stops hearing from it, and a retry is placed elsewhere.
- A worker that comes back reports its jobs, and the leader takes them in.

#### Versions, metrics and job history

A fleet runs mixed fleetci versions while it upgrades, so the versions
say nothing of whether two machines get along. A fleet protocol number
does: each build speaks `Protocol` and serves back to `MinProtocol`. A
controller turns away, with 409, a worker or controller outside that
range, at its join, its status reports and its pings. Such a worker shows
as `Unreachable` and logs why once; a controller is not a peer, so it
takes no part in the election. Upgrade the controllers first, then the
workers. `fleetci fleet status` shows each machine's version.

Every controller keeps the jobs that ended, from the workers' reports,
which go to all of them: the history is whole whichever controller placed
a job, even one that stepped down. Each keeps the last 1000 in
`jobs.jsonl` in its state directory. `fleetci fleet jobs` lists them, with
the VM's boot time and how long the job ran:

```
$ fleetci fleet jobs -n 2
ENDED           WORKER  CLASS      FORGE   JOB       RESULT     BOOT  RAN  ERROR
11-09 17:46:19  w2      1vcpu-2gb  github  many (3)  Succeeded  1s    15s
11-09 17:46:19  w1      1vcpu-2gb  github  many (2)  Succeeded  1s    15s
```

With `[controller] metrics = "127.0.0.1:9482"`, a controller serves
Prometheus metrics at `/metrics`, plain HTTP, so keep it on a private
address. Every controller has the whole fleet's, so scrape any or all;
with more than one, take the max over instances by the labels you keep,
as `docs/grafana/fleetci-fleet.json` does.

| Metric | What |
|---|---|
| `fleetci_fleet_leader`, `fleetci_fleet_controllers_live`, `fleetci_fleet_controller_info{controller,version}` | the controllers |
| `fleetci_fleet_worker_state{worker,state}`, `fleetci_fleet_worker_info{worker,version}` | each worker's state and version |
| `fleetci_fleet_worker_budget_cpus`, `…_budget_memory_bytes`, `…_used_cpus`, `…_used_memory_bytes` | its budget and what its VMs have |
| `fleetci_fleet_worker_room{worker,class}`, `fleetci_fleet_worker_vms{worker,class,job}` | its room and VMs |
| `fleetci_fleet_jobs_total{forge,class,worker,result}` | the jobs that ended |
| `fleetci_fleet_job_duration_seconds{forge,class}`, `fleetci_fleet_vm_boot_seconds{class}` | how long they ran, and their VMs took to boot |
| `fleetci_fleet_vms_total{class,outcome}` | VMs by what came of them: `job`, `unused`, `boot_failed`, `no_room` |
| `fleetci_fleet_leader_changes_total` | hand-overs of the lead this controller saw |
| `fleetci_fleet_queue_wait_seconds{forge,class}` | from GitHub queuing a job to its VM being up; GitLab's runner API gives no queue time |
| `fleetci_github_api_requests_total{code}`, `fleetci_github_api_request_seconds` | the calls to GitHub's API, `code` 0 for no answer |
| `fleetci_github_api_ratelimit_remaining{resource}` | what GitHub's rate limit had left at its last answer |

The last three rows, queue wait and GitHub's API, are the leader's alone: only
it takes jobs. Sum those over the controllers, rather than take the max.

`fleetci fleet verify [worker]` has the workers, or one, run their
microVM checks again, at their next report; `fleetci fleet status -checks`
shows each worker's results. A worker too busy to boot a check VM keeps
what it passed before.

#### Signed releases

A machine runs what it downloads as root, so a checksum from the same
place as the binary is not enough: each release carries `versions.json`,
signed with fleetci's release key, whose public half is built into
fleetci (`internal/release.PublicKey`). It names, per platform, the
fleetci, fleetingd and cloud-hypervisor binaries the release was tested
with, their URLs, sizes and SHA-256, and the fleet protocol range, so one
update moves all three together. fleetingd is re-hosted on fleetci's
release as a plain binary; cloud-hypervisor stays on its own, pinned by
its checksum. The versions are in `hack/release/deps.json`.

The signature is minisign's, so anyone can check a release with the stock
tool:

```
minisign -Vm versions.json -P RWTgbngGzFGS8xCuzg+JIhe87MYOqwyUNqye94EFX+ZJ2sWPDfWFGgYS
```

The release workflow signs with the `FLEETCI_RELEASE_KEY` secret, an
unencrypted minisign key (`go run ./hack/release keygen`); a release made
without it has no manifest, and no machine updates to it.

#### Self-update

A worker with `[update] auto = true` updates itself from these releases, as
far as the controllers let it:

```toml
# worker.toml
[update]
auto = true
# source = "https://api.github.com/repos/nesty92/fleetci/releases"  # or "controllers"
# check_every = "24h"   # jittered by a quarter
# trial = "3m"

# controller.toml
[controller]
worker_update = "minor"  # off (default), patch, minor, latest, or a version to pin
update_parallel = 1      # how many workers update at once
```

The worker looks for the newest release the controllers' policy allows (the
strictest any live controller gives, so a half-upgraded set of controllers
cannot loosen it), checks its `versions.json` signature, and downloads and
checks its binaries into `<state>/update/<version>/`. It then reports
`ready`, and waits for a slot: only the leader hands them out,
`update_parallel` at a time, and none for a release whose protocol the
controllers do not speak. With its slot the worker drains, lets its running
jobs finish, keeps its binaries in `update/previous/`, swaps the new ones in
and re-execs itself.

The new version starts on trial. It commits once it has reached a
controller, every class the old version had passed its microVM check for has
passed again, and `trial` has gone by. It rolls back to the old binaries if
a check fails, if it gets nowhere within 15 minutes, or if it stops during
its trial and is started again. The version it rolled back from is recorded
in `update/failed.json` and never tried again; the worker reports it,
`fleetci fleet status` shows it, and the leader gives no other worker a
slot for it: the rollout stops there. A newer release goes on as usual; to
try the same one again, remove that worker's `failed.json`.
`fleetci fleet update-check` asks every worker to look now instead of at
its next check.

With `source = "controllers"` the worker takes its binaries from the
controllers instead of from GitHub: whatever version they run, over the
connection it already has to them, so no `public_key` is needed and a
fleet with no way out to the internet updates too. Upgrade the controllers
and the workers follow them, under the same policy, slots and trial.

#### Classes: defined once, on the controllers

The classes live in the controllers' `fleetci.toml` and nowhere else. A
class says what a job gets, not where it runs:

```toml
[classes.2vcpu-4gb]
cpus = 2
memory = "4GB"
disk = "30GB"
image = "https://…/fleetci-ubuntu-24.04.qcow2"  # with its checksum in the manifest
overflow = "2vcpu-4gb-aws"

[classes.16vcpu-64gb]
cpus = 16
memory = "64GB"
disk = "100GB"
max_instances = 4                    # optional: a ceiling for the whole fleet
on = { site = "wroclaw" }            # optional: only workers with these labels
```

What used to be per machine in `plugin_config` (the group name, the egress
interface, the VM subnet, the disk directories) each worker works out for
itself, as the join already does.

Each worker reports what it has, when it connects and when it changes:
its architecture, CPUs, memory and free disk, whether KVM works, its budget
(detected less the reserve, as today, or its own `[host]`), its labels and
the cloud credentials it holds. From that the controllers decide which
classes it runs. A class goes to a worker when:

- the class's VM fits in the worker's budget at least once;
- the worker has KVM, for a microVM class, and the image is for its
  architecture;
- its disk holds the image and a snapshot for each of its classes' sizes;
- its labels match the class's `on`, if it has one.

How many is never configured. A worker's classes share its budget, as a
machine's classes share it today, and the host queue decides the mix as
jobs come: a worker with 7 CPUs and 28 GB to give runs three `2vcpu-4gb`,
or one `4vcpu-8gb` and one `2vcpu-4gb`, and is never offered
`16vcpu-64gb`. A class's `max_instances`, if set, caps it across the
fleet, not per machine. `fleetci fleet status` shows each worker's classes
and why a class was left off it ("budget 7 cpus < 16", "no KVM", "label
site").

A change to the classes reaches every worker in the answer to its next
status report. A worker
given a new class builds its snapshot and runs the microVM check for it,
and the class is placed there only once it passes; a class taken away
stops getting jobs, and its running ones finish. The machine's owner can
still narrow what it runs in `worker.toml`, with a `[host]` reserve or a
list of classes to refuse; the controllers never give it more than that.

#### Joining a machine

The first controller is set up once:

```sh
fleetci init                     # creates the fleet's CA and this controller's
                                 # certificate, in /etc/fleetci/pki
fleetci check                    # the forge credentials and the classes, as today
systemctl enable --now fleetci
```

Each new machine then needs one string, made on a controller:

```sh
fleetci token create --ttl 1h --label site=wroclaw
# fleetci join ctl-1.example.org:9481 --token fci1.<CA hash>.<id>.<secret>
```

The token carries the CA's hash, as k3s's does, so the new machine trusts
the controller it dials without anything else copied. On the new machine,
after installing the `fleetci` binary (checksums from the release):

```sh
fleetci join ctl-1.example.org:9481 --token fci1.…
```

`fleetci join` runs as root and prints a line per step, as `fleetci check`
does. It stops at the first failure with the reason, and running it again
picks up where it stopped:

1. **Preflight.** What the machine has: its CPUs, memory, free disk and
   architecture, whether `/dev/kvm` exists and opens (in a cloud VM:
   whether nested virtualisation is on), nftables and `/dev/net/tun`,
   systemd, the clock within a second of the controller's, and the forge
   reachable: `github.com` and the GitHub runner's hosts, or the GitLab
   instance. A missing piece fails only if it leaves the machine no class
   at all; otherwise it is reported and the classes it rules out are left
   off.
2. **Trust.** It fetches the controller's CA, checks it against the hash
   in the token, makes its own key, and gets a certificate signed with the
   token: kubeadm's TLS bootstrap. The token is single-use for a machine
   and expires; the key never leaves the machine.
3. **The fleet's manifest,** from the controller: the fleetci version, the
   Cloud Hypervisor and fleetingd versions with their checksums, the image
   and its checksum, and the classes the controller chose for this machine
   from what preflight found.
4. **Install.** The binaries into `/usr/local/bin`, checked;
   `net.ipv4.ip_forward`; `/etc/fleetci/worker.toml` with what only this
   machine knows, found here: the egress interface (`ip route get`), VM
   subnets that clash with no local route, disk directories, and the
   budget detected as today; and `fleetci-worker.service`, with
   `StateDirectory` for phase 0's state.
5. **The microVM check,** per class given: download the image, build the
   snapshot, boot one microVM from it, and over SSH check that
   - Docker runs, and `docker run` of a small image works, which proves the
     VM's DNS and egress to the registry;
   - the VM reaches the forge: `github.com` and the runner's hosts, or the
     GitLab instance;
   - GitHub classes have actions/runner, at the manifest's version;
   - the boot from the snapshot is fast (it prints the time; under 2 s).
   Then it removes the VM.
6. **Start.** The service starts and connects to every controller. The
   leader places jobs on the worker only once it reports a passing check
   for the versions in the manifest: then it is `Ready`.

After that:

- `fleetci fleet status` on a controller: the controllers and who leads;
  each worker with its state (`Ready`, `NotReady` with the failed check,
  `Unreachable`, `Draining`), its versions, its room and its jobs.
- `fleetci verify` on a worker runs step 5 again. A worker runs it itself,
  and is `NotReady` until it passes, whenever its versions or its image
  change, such as after an upgrade.
- `fleetci drain <worker>` stops placing jobs on it and waits for its jobs;
  the drain is saved and shared with the other controllers, the later of
  a drain and an undrain winning;
  `fleetci leave` on the worker drains it and revokes its certificate, and
  `fleetci revoke <worker>` on a controller does it for a machine that is
  gone.
- A further controller joins with a token made with `--controller`. The
  forge credentials reach it from the leader over mTLS, so they are
  set up once; they are written readable by root only.

#### Cloud classes

A cloud class is defined on the controllers like the others, with its
plugin's settings (the group, the region) but not its credentials, and
`owner = "<worker id>"`. It goes to that worker only, which keeps the
cloud's credentials in its `worker.toml` and reports holding them; a
controller that is also a worker can be it. Each
cloud group still has exactly one owner, so problem 7 never comes up, and
phase 0 keeps its jobs across that worker's restarts. Losing that worker
loses the class until it comes back, or until another worker is given the
class; jobs of the class wait, or go to another overflow. Moving a cloud
class to another worker by itself is left for later (see To decide).

#### Steps

1. **Two spikes.** GitHub's 409 on a real repo and an org: the same name
   twice at once from two clients, and again after the first runner ended.
   A GitLab job taken with the runner token in one process and run to the
   end in another with only the job's token: logs, artifacts, cache,
   cancel.
2. **The roles in one process.** `serve` split into a controller and a
   worker in one process, with the host queue's hosts being
   workers. A single machine runs both, unchanged for the user.
3. **The API over mTLS,** with `fleetci init`, `token create`, `join`
   (steps 1, 2 and 6) and `fleet status`. Workers installed by hand from
   the runbook can join from here.
4. **Install and the microVM check:** the manifest, steps 3 to 5 of the
   join, `fleetci verify`, and `Ready`.
5. **Standbys:** the election, a new leader rebuilding from the workers and
   GitHub's runner list, and the forge credentials sent to a joining
   controller.
6. **The rest of phase 0,** alongside: the GitLab drain, then fleetingd
   adoption.

## Tests

- **In one process:** two or three controllers and N workers on in-memory
  streams that drop, delay and cut, with a fake GitHub (`forge_test.go`
  already keeps names unique), a fake GitLab that gives each job once, and
  fake plugins, under `internal/chaos` fault schedules: the leader killed
  or stopped, workers lost and back, the forge slow or erring. After each
  run:
  - no job got runners, or builds, on two workers at once;
  - no worker ran past its budget;
  - no job was placed on a worker its class doesn't fit, or before the
    class passed its microVM check there;
  - no job waited past `claim_timeout` while a `Ready` worker had room for
    it and a leader was up;
  - every job taken started, or failed with its reason;
  - after a takeover, the new leader's view of the jobs matches the
    workers'.
- **The join, on clean machines:** `fleetci join` on fresh Ubuntu and
  Debian VMs with nested KVM, and on one without it, which must stop at
  preflight with the reason; run again after each step is interrupted.
- **The demo:** three to five Cloud Hypervisor microVMs from the local kit,
  with nested KVM, joined with `fleetci join`: one controller with a public
  port, the workers each behind their own NAT, reaching only it and the
  forge, all on the same GitHub repos and GitLab project.
- **Chaos:** kill -9 and SIGSTOP of the leader and of workers; a worker cut
  from the controllers but not from the forge; a site cut off with its own
  controller; rolling upgrades with two versions side by side; an expired
  join token; a revoked worker certificate.

## To decide

1. **The port:** 9481 (9480 is the webhook's in the examples).
2. **Forge credentials on a joining controller:** sent by the leader, as
   above, or copied by hand, so a stolen controller token can't fetch
   them.
3. **The microVM check's image:** a small image from the manifest's
   registry, or one built into fleetci's image, so the check needs no
   registry and the registry is checked separately.
4. **Upgrades:** workers that update themselves to the manifest's versions
   and verify before they are `Ready` again, or upgrades left to the
   machine's package tooling, with `NotReady` until they match.
5. **A cloud class that moves** to another worker when its owner is lost:
   its taskscaler state sent to the controllers, and a new owner adopting
   its VMs as in phase 0, fenced so the old owner, if it comes back,
   stops first.
6. **Gossip among controllers,** where they share a network, so standbys
   see each other's room and links faster; the election works without it.
7. **Disk for snapshots:** a snapshot per class size on every worker
   that runs it, or only the sizes a worker is busiest with, built on
   demand for the rest at the cost of a slower first job.
8. **fleetingd adoption:** in the next round, or left, with on-host VMs
   lost on a restart for now.
