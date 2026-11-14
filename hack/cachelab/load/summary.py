#!/usr/bin/env python3
"""What a load run measured, as GitLab and GitHub's runner fleets report it.

    summary.py <run>...

For each run, first what the cache changes: each job's time once
started, at p50/p95, the share of the time each phase took, and what the
jobs' caches started from. Then the queue, which more runners shorten
and the cache does not: its p50/p95/p99, the share of jobs that waited
30 s or less (GitLab.com's target for its shared runners is 95 % of
them), and, from the controller's metrics, fleetci's own part of it (the
pickup: GitHub queuing a job to fleetci taking it, by poll or webhook)
apart from the wait for room. Last, the workers' cache counters over the
run, and the most each worker's caches took.
"""
import json
import os
import re
import sys

OUT = os.path.expanduser("~/.local/share/fleetci-cachelab/load")
SAMPLE = re.compile(r'^([a-z_]+)(\{[^}]*\})? ([0-9.e+-]+|NaN|\+Inf)$')


def pct(xs, p):
    xs = sorted(x for x in xs if x is not None)
    if not xs:
        return None
    k = (len(xs) - 1) * p / 100
    i = int(k)
    return xs[i] + (xs[min(i + 1, len(xs) - 1)] - xs[i]) * (k - i)


def fmt(x):
    return "-" if x is None else f"{x:.0f}s"


def scrape(path):
    out = {}
    try:
        with open(path) as f:
            for line in f:
                m = SAMPLE.match(line.strip())
                if m:
                    out[m[1] + (m[2] or "")] = float(m[3])
    except OSError:
        pass
    return out


def disk_from(rec):
    """What the job's cache disk started from, as its log says: empty, a
    fallback's (main's), or its own; none without a disk."""
    for line in rec["lines"]:
        if line.startswith("Cache disk "):
            if ": empty" in line:
                return "empty"
            return "fallback" if ": starting from" in line else "own"
    return "none"


def run(name):
    recs = []
    with open(os.path.join(OUT, f"{name}.jsonl")) as f:
        for line in f:
            recs.append(json.loads(line))
    load = [r for r in recs if r["kind"] != "base"]
    if not load:
        print(f"{name}: no jobs after the base")
        return
    r0 = load[0]
    print(f"== {name}: {r0['forge']} {r0['mode']}, {len(load)} jobs, {sum(not r['ok'] for r in load)} failed")
    for job in sorted({r["job"] for r in load}):
        js = [r for r in load if r["job"] == job and r["ok"]]
        wall = [r["wall"] for r in js]
        phases = {}
        for r in js:
            for k, v in r["phases"].items():
                phases[k] = phases.get(k, 0) + v
        total = sum(r["wall"] or 0 for r in js) or 1
        top = sorted(phases.items(), key=lambda kv: -kv[1])[:6]
        print(f"{job:4} wall p50 {fmt(pct(wall, 50))} p95 {fmt(pct(wall, 95))}; "
              + ", ".join(f"{k} {v / total:.0%}" for k, v in top))
        froms = {}
        for r in js:
            froms[disk_from(r)] = froms.get(disk_from(r), 0) + 1
        print(f"     cache from: " + ", ".join(f"{k} {v}" for k, v in sorted(froms.items())))
    q = [r["queued"] for r in load]
    within = sum(1 for x in q if x is not None and x <= 30) / max(1, sum(x is not None for x in q))
    print(f"queue  p50 {fmt(pct(q, 50))}  p95 {fmt(pct(q, 95))}  p99 {fmt(pct(q, 99))}  within 30s {within:.0%}"
          " (capacity: more runners shorten it)")
    a, b = scrape(os.path.join(OUT, f"{name}.metrics-start")), scrape(os.path.join(OUT, f"{name}.metrics-end"))
    if not b:
        print("no metrics scraped")
        return
    for key in sorted(b):
        if key.startswith(("fleetci_fleet_worker_cache_evictions_total", "fleetci_fleet_worker_cache_flattens_total",
                           "fleetci_fleet_worker_cache_copied_bytes_total", "fleetci_fleet_job_cache_total")):
            d = b[key] - a.get(key, 0)
            if d:
                print(f"     {key} +{d:.0f}")
        elif key.startswith("fleetci_fleet_worker_cache_used_bytes"):
            print(f"     {key} {b[key] / 2**30:.1f} GiB")
    for forge in ("github", "gitlab"):
        sums = [(k, b[k] - a.get(k, 0)) for k in b if k.startswith("fleetci_fleet_queue_wait_seconds_sum") and f'forge="{forge}"' in k]
        cnts = [(k, b[k] - a.get(k, 0)) for k in b if k.startswith("fleetci_fleet_queue_wait_seconds_count") and f'forge="{forge}"' in k]
        n = sum(c for _, c in cnts)
        if n:
            print(f"     controller's queue wait, {forge}: mean {sum(s for _, s in sums) / n:.0f}s over {n:.0f} jobs")
    for via in ("poll", "webhook"):
        sel = lambda k, part: k.startswith(f"fleetci_fleet_pickup_seconds_{part}") and f'via="{via}"' in k
        n = sum(b[k] - a.get(k, 0) for k in b if sel(k, "count"))
        if n:
            mean = sum(b[k] - a.get(k, 0) for k in b if sel(k, "sum")) / n
            print(f"     pickup by {via} (fleetci's own part of the queue): mean {mean:.1f}s over {n:.0f} jobs")


if __name__ == "__main__":
    for name in sys.argv[1:]:
        run(name)
