# Spot hosts that the cloud takes back

Status: steps 1 to 3 are on `feat/spot-reclaim`; the rest is plan.

Issues: #6 (stop placing VMs on a host marked for reclaim), #8 (drain a
host on a rebalance recommendation), #7 (choose zones from reclaim history
and placement scores).

## The rule

fleetci never stops a running job to make room for the cloud. When a host
is about to go, fleetci stops placing new VMs on it (cordons it), gets a
replacement going, and removes the host once its last job ends. If the
cloud takes the host before that, the jobs on it die: that is the cloud's
decision, and how long a host may wait is the user's setting (below), not
fleetci's.

## What AWS tells us, and when

| Signal | Warning | Where to read it |
|---|---|---|
| Spot interruption notice | 2 minutes | IMDS `spot/instance-action`; EventBridge "EC2 Spot Instance Interruption Warning"; `DescribeSpotInstanceRequests` status |
| Rebalance recommendation | usually more than 2 minutes, often much more; may never be followed by an interruption | IMDS `events/recommendations/rebalance`; EventBridge "EC2 Instance Rebalance Recommendation" |
| Scheduled maintenance | days | IMDS `events/maintenance/scheduled`; EventBridge "AWS Health Event" |
| ASG wants the instance gone (scale-in, Capacity Rebalancing, instance refresh) | as long as a termination lifecycle hook holds it | IMDS `autoscaling/target-lifecycle-state`; the plugin's `Update` reports `Terminating*` as `StateDeleting` |

## What others do

**fleeting's AWS plugin.** Its `Heartbeat` looks the instance up with
`DescribeSpotInstanceRequests` (batched across instances) and fails with
`ErrSpotInstanceTerminationNotice` once the request leaves "fulfilled". It
turns the check off when the IAM permission is missing. taskscaler only
calls `Heartbeat` when given a heartbeat function, and gitlab-runner only
gives one behind `FF_USE_FLEETING_ACQUIRE_HEARTBEATS` (off by default).
fleetci gave none, so the check never ran: that is #6.

**Karpenter** (`pkg/controllers/interruption`). An SQS queue fed by
EventBridge, plus a poller of `DescribeInstanceStatus`. Spot interruption,
scheduled change, stop and terminate events cordon and drain the node;
failed status checks remove it at once; rebalance recommendations are only
recorded. On a spot interruption, and on an insufficient-capacity error
from `CreateFleet`, it marks the (instance type, zone, spot) offering
unavailable for three minutes (`UnavailableOfferingsTTL`) so the
replacement goes somewhere else. The replacement is ordinary provisioning:
the pods that lost their node are pending again. Draining evicts pods,
which fleetci does not do.

**aws-node-termination-handler.** Two modes. IMDS mode is a daemon on each
node polling the four IMDS paths above every 2 seconds; on a rebalance
recommendation it only cordons. Queue mode reads the same EventBridge
events from SQS, and also handles ASG termination lifecycle hooks: it
keeps the hook alive with heartbeats while the node drains, then completes
it.

Karpenter and NTH are Apache-2.0. These are ideas, not code; if any of
their code is ported it carries their notice.

## The design

### Signals: the host itself by default, a queue as an option

By default fleetci reads each host's IMDS every 2 seconds, as NTH's IMDS
mode does, but from outside the host: fleetci holds one SSH connection to
the host and opens a TCP forward (`direct-tcpip`) through it to
`169.254.169.254:80`. The host's sshd makes the connection, so IMDS
answers as it would to the host itself, and fleetci speaks IMDSv2 over it:
a session token, then `spot/instance-action` and
`events/recommendations/rebalance` (later
`autoscaling/target-lifecycle-state`). Nothing runs on the host, so the
image needs no curl or shell, and no IAM is added. The one requirement is
that sshd allows TCP forwarding, its default; jobs already reach Docker
through a forward. A host whose sshd refuses, or that has no IMDS, is
logged once and left to the plugin's own check. A lost connection is
opened again with backoff; one EC2 Instance Connect key per connection,
not per read.

The watch gives up on a host only when the host shows it has no IMDS:
sshd refuses the forward, sshd can't reach the address, or, before IMDS
has ever answered, three reads in a row time out or get an answer that
isn't IMDSv2's. A 5xx or a 429 is IMDS busy or restarting, and a
connection that resets is trouble on the way; neither is IMDS absent, so
the watch keeps reading, however early they come, and a host whose IMDS
hiccups while it boots is still watched. A token IMDS no longer knows (401) is
renewed once.

That connection is the host's only one. The watch and every job on the
host share it, and each opens its own sessions and forwards on it: the
plugin-mode proxy, the standalone GitLab executor (a `connector.Client`
over it, ported from fleeting's) and the GitHub runner. A job ends its own
sessions and leaves the connection; it closes when the host is removed.
The checks that admit a host still log in afresh, so the login a job gets
already has the groups setup gave the user.

The watch starts once a host is admitted (Docker answers) and ends when
the host is removed. It is on by default for the AWS plugin
(`[classes.<name>.reclaim] watch`).

A class may instead name an SQS queue that EventBridge feeds, as Karpenter
and NTH's queue mode do. It needs the queue, the rules and
`sqs:ReceiveMessage`/`sqs:DeleteMessage`, but no traffic to hosts and it
also sees events for hosts fleetci cannot reach. The queue is opt-in per
class; both feed the same cordon below.

The plugin's own `Heartbeat` check stays on as a third, slower source
(step 1).

### Cordon

A cordoned host takes no new VM. Its running VMs carry on. When it holds
none, fleetci removes it. taskscaler already does this for two cases:

- an instance whose `Heartbeat` fails is skipped by `Acquire`, and is
  removed once it is idle and has failed `FailureThreshold` (3) times;
- an instance reported `StateDeleting` is marked unready: nothing new
  lands on it, running work continues.

So the IMDS and queue signals reach taskscaler as a failing `Heartbeat`
for that instance, through the `supervised` wrapper in
`internal/fleet/supervise.go`. No fork of taskscaler is needed.

### Replacement

A cordoned host's slots no longer count as free, so a class with idle
capacity scales up at once; a class without gets the next host when a job
arrives, as today. A rebalance recommendation usually comes early enough
for the replacement to be up before the old host goes.

A rebalance recommendation does nothing by default, as in Karpenter: it
is often not followed by an interruption, and acting on it costs a new
host each time. A class opts in with a setting, and then the
recommendation cordons the host like an interruption notice does.

The replacement should not land where the old host was reclaimed.
Following Karpenter, fleetci keeps a short-lived list of (type, zone)
pairs to avoid: three minutes after an interruption notice, or after an
insufficient-capacity error. With one class in one ASG fleetci can't
steer the zone; this becomes useful with #7's one pool per zone.

### Holding a host the ASG wants to remove

Scale-in and Capacity Rebalancing terminate instances without asking
fleetci. A termination lifecycle hook on the ASG makes them wait in
`Terminating:Wait`. The plugin reports that as `StateDeleting`, so the
host is cordoned. fleetci sends `RecordLifecycleActionHeartbeat` while
the host still has jobs and `CompleteLifecycleAction` (CONTINUE) when it
is empty. The hook's timeout, and the 48-hour cap AWS puts on any hook, is
how long a job may hold a host: the user sets it on the ASG, and
`hack/aws` documents it. This needs the two `autoscaling:` permissions and
is opt-in.

This does not apply to a spot interruption: the two minutes are fixed.

### Choosing zones (#7)

Later. First the avoid-list above. Then one pool per zone under one class,
ordered by recent reclaims in that zone and by
`GetSpotPlacementScores`, with the class falling over to the next zone
the way it falls over to its overflow class today.

## Order

1. Turn on taskscaler's heartbeat, so the plugin's spot check runs before
   every placement (#6).
2. IMDS through an SSH tunnel: cordon on an interruption notice (#6).
3. Cordon on a rebalance recommendation, when the class asks for it
   (`reclaim.rebalance`, #8).
4. Lifecycle hook support (#8).
5. SQS queue as an opt-in source.
6. Avoid-list, then zones (#7).

Steps 1 to 3 are on `feat/spot-reclaim`, with the one shared connection
per host. They are tested against a fake IMDS behind a real SSH server,
not yet on EC2.

## Testing under faults

Besides the tests that pin each behaviour, fuzz tests put the shared
connection, the reclaim watch and the jobs' commands (GitLab and GitHub)
under faults that `internal/chaos` injects. A TCP proxy between fleetci and
the host's sshd refuses, cuts, black-holes, slows or chops each
connection, at once or after a while, and a fake IMDS answers 5xx, 429, a
lost token or late. The fuzz input is the plan: each byte one decision. A
test drives the code through it, makes the network good again, and checks
what must hold whatever happened: one dial at a time, no call past its
deadline, no notice made up, no truncated output reported as success, the
host reachable again once the network is, and nothing left open after
close.

The GitHub dispatcher gets the same treatment from the other side: a fake
GitHub that keeps state as GitHub does (queued jobs, registered runners, a
runner taking any job with its labels) sits behind the proxy and answers
some requests with 502, a rate limit, an answer lost after it acted, a late
answer or one cut halfway. Once things are good again every job must have
run once, no runner be left registered, and every VM be released. It found
that a runner taking another job's place left that job unserved for good.

`go test` runs the written plans and four random ones each time; `go test
-fuzz FuzzReclaimWatchUnderChaos ./internal/fleet/` searches longer. A
failure prints the scenario, each fault with when it struck, and the
`CHAOS_PLAN=<hex>` that replays it.
