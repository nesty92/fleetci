# Metrics

fleetci serves Prometheus metrics in all three ways it runs:

| Runs as | Where | Dashboard |
| --- | --- | --- |
| `fleetci serve` | `metrics_listen` in fleetci.toml | [`grafana/fleetci.json`](grafana/fleetci.json) |
| gitlab-runner's fleetci plugin | `metrics_listen` in fleetci.toml | [`grafana/fleetci.json`](grafana/fleetci.json) |
| a fleet (controllers and workers) | each controller's `/metrics` (`[controller] metrics`) | [`grafana/fleetci-fleet.json`](grafana/fleetci-fleet.json) |

`metrics_listen` is an address such as `127.0.0.1:9483`; fleetci serves plain
HTTP at `/metrics` there. Without it nothing is served.

## In plugin mode

gitlab-runner has fleeting and taskscaler metrics of its own at its
`listen_address`. With fleetci as its plugin they describe one instance
group of one instance: fleetci's gateway. The classes, their VMs and their waits are only in
fleetci's metrics, at `metrics_listen`. Scrape both: gitlab-runner's still
has what is gitlab-runner's, such as `gitlab_runner_jobs` and
`gitlab_runner_job_queue_duration_seconds`.

gitlab-runner starts one plugin process for each `[[runners]]` entry that
uses fleetci, and keeps it until it stops; reloading config.toml keeps the
process. Each entry needs a `metrics_listen` of its own, in its
`plugin_config` or its `config_file`, or the second one's fleetci fails to
start.

## The classes

Kept by every `fleetci serve` and plugin; a fleet's workers have their own
(the `fleetci_fleet_*` metrics).

| Metric | Type | What |
| --- | --- | --- |
| `fleetci_class_jobs{class,state}` | gauge | jobs `running` on a VM, or `waiting` for one: for room, or for one to boot |
| `fleetci_class_slots{class,state}` | gauge | taskscaler's job slots: `idle` (ready for a job), `pending` (booting), `acquired`, `reserved`, `unavailable`, `unhealthy`, `suspended` |
| `fleetci_class_instances{class,state}` | gauge | the class's instances as fleeting counts them: `requested`, `pending`, `creating`, `running`, `deleting` and others |
| `fleetci_class_max_instances{class}` | gauge | `max_instances` |
| `fleetci_class_draining_vms{class}` | gauge | VMs with a reclaim notice: they take no new jobs and go once their jobs end |
| `fleetci_vm_wait_seconds{class}` | histogram | from a job asking for a VM of its class to having one; `class` is the class asked for, also when the overflow served it |
| `fleetci_vms_acquired_total{class,start}` | counter | VMs jobs got, `warm` (up before the job asked: `idle_count`, or a VM a cancelled wait left) or `cold` (booted for it) |
| `fleetci_vm_acquire_failures_total{class,reason}` | counter | jobs that got no VM: `timeout` (`acquire_timeout` passed), `cancelled` (the job stopped waiting) or `error` |
| `fleetci_overflow_total{class,to}` | counter | jobs of a full class that ran on its overflow class |
| `fleetci_nominations_total{class}` | counter | jobs that waited `nominate_after` and had a host hold room for them |
| `fleetci_vm_held_seconds{class}` | histogram | from a job getting its VM to giving it back |
| `fleetci_instance_boot_seconds{class}` | histogram | from asking the plugin for an instance to the plugin reporting it running |
| `fleetci_instance_ready_seconds{class}` | histogram | from running to admitted: connecting to it, and fleetci's checks of Docker and the registry mirrors |
| `fleetci_instance_lifetime_seconds{class}` | histogram | from running to deleted |
| `fleetci_class_instance_operations_total{class,operation}` | counter | instances created, deleted and so on |
| `fleetci_class_scale_operations_total{class,operation}` | counter | taskscaler scaling up or down |
| `fleetci_instances_lost_total{class}` | counter | instances gone without fleetci removing them: VMs that died, spot instances taken back |
| `fleetci_reclaim_notices_total{class,kind}` | counter | the cloud's notices, `reclaim` or `rebalance`, once per VM |

The host's budget, and the cache disks:

| Metric | Type | What |
| --- | --- | --- |
| `fleetci_host_budget_cpus{host}`, `fleetci_host_budget_memory_bytes{host}` | gauge | what the host lends its classes' VMs (`[host]`) |
| `fleetci_host_used_cpus{host}`, `fleetci_host_used_memory_bytes{host}` | gauge | what of it is taken: VMs, and reserved classes' fixed shares |
| `fleetci_cache_used_bytes`, `fleetci_cache_budget_bytes` | gauge | the cache disks' size and what they may take |
| `fleetci_cache_low_space` | gauge | 1 while the filesystem is under `min_free`: jobs get no cache disk |
| `fleetci_cache_evictions_total`, `fleetci_cache_flattens_total` | counter | caches dropped and chains flattened |

## The forges

| Metric | Type | What |
| --- | --- | --- |
| `fleetci_queue_wait_seconds{forge,class}` | histogram | GitHub: from GitHub queuing the job to its VM being up |
| `fleetci_pickup_seconds{forge,class,via}` | histogram | GitHub: from GitHub queuing the job to fleetci first taking it, by `poll` or `webhook` |
| `fleetci_github_api_requests_total{code}` | counter | calls to GitHub's API by HTTP status, `0` for no answer |
| `fleetci_github_api_request_seconds` | histogram | how long GitHub took to answer |
| `fleetci_github_api_ratelimit_remaining{resource}` | gauge | what the rate limit had left, as its last answer said |
| `gitlab_runner_*` | | `fleetci serve` with `[gitlab]`: gitlab-runner's own, as its `listen_address` would serve them, such as `gitlab_runner_job_queue_duration_seconds` (GitLab's queue until fleetci took the job) |

A fleet's controllers name the two histograms `fleetci_fleet_queue_wait_seconds`
and `fleetci_fleet_pickup_seconds`, and have them for GitLab too.

For GitLab outside a fleet, the queue wait is in two parts: GitLab's queue,
`gitlab_runner_job_queue_duration_seconds`, then the wait for a VM,
`fleetci_vm_wait_seconds`.

## Job sizing

`fleetci_job_*`: see [job-usage.md](job-usage.md#metrics).

## Queries

The wait for a VM, p95 by class:

```promql
histogram_quantile(0.95, sum by (le, class) (rate(fleetci_vm_wait_seconds_bucket[5m])))
```

What `idle_count` buys, the share of jobs whose VM was up:

```promql
sum by (class) (increase(fleetci_vms_acquired_total{start="warm"}[1d]))
  / sum by (class) (increase(fleetci_vms_acquired_total[1d]))
```

A class short of VMs, jobs waiting for ten minutes:

```promql
min_over_time(sum by (class) (fleetci_class_jobs{state="waiting"})[10m:]) > 0
```
