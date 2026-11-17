#!/usr/bin/env python3
"""What a load run's jobs cost, per job, on either arm.

    cost.py host <run> --host-price USD/h [--host-disk-gb 200]
    cost.py ec2  <run> --group NAME=USD/h [--group …] [--vm-disk-gb 30]

<run> is a load.py run, read from $LOAD_OUT/<run>.jsonl (the jobs and
their times). Prices are on-demand USD per hour for the region; look them
up, they change (README, "Cost").

host: the host was paid for from the run's first job queued to its last
job ended, whatever the microVMs did; that span's cost over the jobs.

ec2: each baseline VM was paid for from its launch to its termination, per
second with a minute's minimum (Linux on-demand). The times come from
the Auto Scaling groups' activity history, which AWS keeps for six
weeks; only the VMs launched within the run's span count. Its gp3 disk is
added at $0.08 per GB-month, the us-east-1 price.

Run it with the same AWS_PROFILE and AWS_REGION as the rest; it only reads.
"""
import argparse
import datetime
import json
import os
import re
import subprocess
import sys

OUT = os.path.expanduser(os.environ.get("LOAD_OUT", "~/.local/share/fleetci-cachelab/load"))
GB_MONTH = 0.08
MONTH_S = 730 * 3600


def jobs(run):
    with open(os.path.join(OUT, f"{run}.jsonl")) as f:
        recs = [json.loads(l) for l in f if l.strip()]
    recs = [r for r in recs if r.get("created") and r.get("ended")]
    if not recs:
        sys.exit(f"{run}: no finished jobs in {OUT}/{run}.jsonl")
    return recs


def when(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def activities(group):
    out, token = [], None
    while True:
        cmd = ["aws", "autoscaling", "describe-scaling-activities", "--auto-scaling-group-name", group, "--output", "json"]
        if token:
            cmd += ["--starting-token", token]
        r = json.loads(subprocess.run(cmd, check=True, capture_output=True, text=True).stdout)
        out += r.get("Activities", [])
        token = r.get("NextToken")
        if not token:
            return out


def vms(group, t0, t1):
    """Each instance's launch and termination in the group, launched in [t0, t1]."""
    up, down = {}, {}
    for a in activities(group):
        m = re.search(r"(i-[0-9a-f]+)", a.get("Description", ""))
        if not m or a.get("StatusCode") != "Successful":
            continue
        i = m.group(1)
        if a["Description"].startswith("Launching"):
            up[i] = when(a["StartTime"])
        elif a["Description"].startswith("Terminating"):
            down[i] = when(a.get("EndTime") or a["StartTime"])
    return {i: (t, down.get(i)) for i, t in up.items() if t0 - 300 <= t <= t1}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("arm", choices=["host", "ec2"])
    ap.add_argument("run")
    ap.add_argument("--host-price", type=float)
    ap.add_argument("--host-disk-gb", type=float, default=200)
    ap.add_argument("--group", action="append", default=[])
    ap.add_argument("--vm-disk-gb", type=float, default=30)
    a = ap.parse_args()

    recs = jobs(a.run)
    t0, t1 = min(r["created"] for r in recs), max(r["ended"] for r in recs)
    n, busy = len(recs), sum(r["wall"] or 0 for r in recs)
    print(f"{a.run}: {n} jobs over {(t1 - t0) / 3600:.2f} h, {busy / 3600:.2f} job-hours")

    if a.arm == "host":
        if a.host_price is None:
            sys.exit("--host-price is needed")
        span = t1 - t0
        usd = span / 3600 * a.host_price + span / MONTH_S * a.host_disk_gb * GB_MONTH
        print(f"host  ${usd:.3f} for the span: ${usd / n:.4f} a job")
        return

    if not a.group:
        sys.exit("--group NAME=USD/h is needed, once per baseline group")
    total, count = 0.0, 0
    for g in a.group:
        name, price = g.split("=", 1)
        seen = vms(name, t0, t1)
        secs = 0.0
        for i, (up, down) in seen.items():
            if down is None:
                print(f"  {name} {i}: not terminated yet; counted to now", file=sys.stderr)
                down = datetime.datetime.now(datetime.timezone.utc).timestamp()
            secs += max(60.0, down - up)
        usd = secs / 3600 * float(price) + secs / MONTH_S * a.vm_disk_gb * GB_MONTH
        total, count = total + usd, count + len(seen)
        print(f"  {name}: {len(seen)} VMs, {secs / 3600:.2f} VM-hours, ${usd:.3f}")
    print(f"ec2   ${total:.3f} for {count} VMs: ${total / n:.4f} a job")


if __name__ == "__main__":
    main()
