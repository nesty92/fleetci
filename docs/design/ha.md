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
   streams    │   │           │
   opened by  │   │           │
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
- **Workers** hold no forge credentials. Each owns its budget, its
  fleetingd classes and its VMs, and tells the controllers its room. A
  worker can also own a cloud class, with that cloud's credentials.

#### The connection: workers dial out

Each worker opens one connection to every controller and keeps it: gRPC
over mutual TLS, on one TCP port (9481), with keepalives. Only the
controllers need to be reachable; workers can be behind any NAT, as k3s
agents are. Controllers dial each other on the same port.

On the stream, the worker sends:
- **its room,** per class, when it changes and at least every 5 s; that is
  also its heartbeat;
- **its jobs,** each with its forge, job id and runner name, so a new
  leader knows what runs where;
- **answers** to the leader's requests.

And the leader sends:
- **`Run`:** a GitHub job, with its class and the runner's JIT config; or a
  GitLab job, with the job as GitLab gave it and the `[[runners]]` table it
  runs under.
- **`Drain`, `Cancel`** and config changes.

A worker decides `Run` itself: it reserves room and answers yes, or says
no room. So a leader working from stale room, or two leaders at once,
can't overbook a worker: they get "no room" and try another.

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
  stream allows: only a controller may send `Run`, and a worker may only
  report about itself, under the id in its certificate, whatever the
  message says.
- **Keys never move.** Each machine makes its own key at join and sends a
  CSR; the controller signs it. The bootstrap token only authorises that
  one signing, as kubeadm's does: it is single-use per machine, expires
  (1 h by default), and is the only thing the join port accepts without a
  certificate.
- **Short certificates, renewed on the stream.** A worker's certificate
  lasts 30 days, and the worker asks for a new one over its mTLS stream
  after two thirds of that, with a new key. A worker off for longer joins
  again with a new token.
- **Revocation is a denylist** the controllers share and check on every
  handshake and on open streams: `fleetci leave` and
  `fleetci revoke <worker>` add the certificate's serial, and its streams
  close at once. Short lifetimes keep the list small; an entry goes when
  its certificate would have expired.
- **The CA rotates** by overlap: `fleetci ca rotate` adds a new CA, the
  controllers trust both and sign with the new one, workers pick up the
  new chain on renewal, and the old CA is dropped once no live
  certificate uses it.

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
   worker with the job, over the stream, and stay in memory.

The time a job spends on the leader is one placement, milliseconds. A
leader that dies in it leaves a job GitLab thinks is running and nobody
runs; GitLab fails it when it goes stale. That is the one window, and it
needs a crash at that moment.

#### Leader and standbys

The controllers pick the leader with no consensus: the live controller
with the lowest id leads, where live is a heartbeat seen in the last 10 s.
That needs no quorum and no third machine, and a site cut off with a
controller elects its own and keeps working.

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

A worker's stream that misses its heartbeats for 15 s marks it
`Unreachable`. Nothing placed on it is run again yet: its runners and
GitLab builds may still be running and reaching the forge.

- **GitHub:** a job whose runner never came online is tried again under
  `<try>+1` after `claim_timeout` (3 minutes). A job in progress is left to
  GitHub: it finishes, or GitHub fails it when its runner is lost.
- **GitLab:** the job is the worker's; it finishes, or GitLab fails it when
  it stops hearing from it, and a retry is placed elsewhere.
- A worker that comes back reports its jobs, and the leader takes them in.

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
fleetci token create --ttl 1h --classes 4vcpu-8gb,2vcpu-4gb
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

1. **Preflight.** `/dev/kvm` exists and opens (in a cloud VM: nested
   virtualisation is on); the CPU and memory the classes need; free disk
   for the image and a snapshot per class; nftables and `/dev/net/tun`;
   systemd; the clock within a second of the controller's; and the forge
   reachable: `github.com` and the GitHub runner's hosts, or the GitLab
   instance.
2. **Trust.** It fetches the controller's CA, checks it against the hash
   in the token, makes its own key, and gets a certificate signed with the
   token: kubeadm's TLS bootstrap. The token is single-use for a machine
   and expires; the key never leaves the machine.
3. **The fleet's manifest,** from the controller: the fleetci version, the
   Cloud Hypervisor and fleetingd versions with their checksums, the image
   and its checksum, and the classes this machine serves.
4. **Install.** The binaries into `/usr/local/bin`, checked;
   `net.ipv4.ip_forward`; `/etc/fleetci/worker.toml` with what only this
   machine knows, found here: the egress interface (`ip route get`), VM
   subnets that clash with no local route, disk directories, and the
   budget detected as today; and `fleetci-worker.service`, with
   `StateDirectory` for phase 0's state.
5. **The microVM check,** per class: download the image, build the
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
  `fleetci leave` on the worker drains it and revokes its certificate, and
  `fleetci revoke <worker>` on a controller does it for a machine that is
  gone.
- A further controller joins with a token made with `--controller`. The
  forge credentials reach it from the leader over the stream, so they are
  set up once; they are written readable by root only.

#### Cloud classes

A cloud class belongs to one worker, the one with that cloud's credentials
in its `worker.toml`; a controller that is also a worker can be it. Each
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
   worker over an in-memory stream, with the host queue's hosts being
   workers. A single machine runs both, unchanged for the user.
3. **The stream over mTLS,** with `fleetci init`, `token create`, `join`
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
7. **fleetingd adoption:** in the next round, or left, with on-host VMs
   lost on a restart for now.
