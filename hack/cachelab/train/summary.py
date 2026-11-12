#!/usr/bin/env python3
"""Where a train's time went, from drive.py's <run>.jsonl files.

    summary.py <run>...

For each run: per job, its wall time from start to finish against the time
its own CI took (the TRAIN line's total), so the rest is the VM, the image,
the checkout and the cache's restore and save; and the cache lines.
"""
import datetime
import json
import os
import re
import sys

OUT = os.path.expanduser("~/.local/share/fleetci-cachelab/train")


def t(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))


for run in sys.argv[1:]:
    recs = [json.loads(l) for l in open(os.path.join(OUT, run + ".jsonl"))]
    print(f"== {run}: {recs[0]['forge']} {recs[0]['mode']}")
    tot = {}
    for r in recs:
        wall = (t(r["finished"]) - t(r["started"])).total_seconds()
        train = [l for l in r["lines"] if l.startswith("TRAIN ")]
        own = float(re.search(r"total=([\d.]+)", train[-1]).group(1)) if train else 0
        rest = [l for l in r["lines"] if not l.startswith("TRAIN ")]
        hit = "hit" if any("restored from key" in l or "Successfully extracted" in l or "fleetci cache:" in l and "disk" in l
                           for l in rest) else "miss"
        k = (r["job"], "base" if r["car"] == 0 else r["ref"].split("/")[0])
        a = tot.setdefault(k, [0, 0, 0])
        if r["car"]:
            a[0] += 1
            a[1] += wall
            a[2] += own
        print(f"  car {r['car']} {r['ref']:<20} {r['job']:<4} wall {wall:5.0f}s  own {own:5.1f}s  other {wall - own:5.0f}s  {hit}"
              f"  {r.get('runner') or ''}")
        for l in rest:
            if "fleetci cache:" in l:
                print("      " + l[:200])
    for (job, where), (n, wall, own) in sorted(tot.items()):
        if n:
            print(f"  mean {job} on {where}: wall {wall / n:.0f}s, own {own / n:.0f}s, other {(wall - own) / n:.0f}s over {n}")
