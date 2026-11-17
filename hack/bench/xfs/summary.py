"""Medians of the bench's results, as a Markdown table per VM count."""
import csv, statistics, sys
from collections import defaultdict

rows = list(csv.DictReader(open(sys.argv[1]), delimiter="\t"))
vals = defaultdict(list)
for r in rows:
    try:
        vals[(r["vms"], r["key"], r["disk"], r["cache"])].append(float(r["value"]))
    except ValueError:
        pass
keys = ["make_disk_ms", "boot_ms", "read_tree_ms", "read_big_ms", "copy_tree_ms", "delete_tree_ms",
        "randwrite_iops", "randwrite_p99_us", "syncwrite_iops", "wall_ms", "host_io_wait_ms"]
higher = {"randwrite_iops", "syncwrite_iops"}
cols = [(d, c) for c in ("warm", "cold") for d in ("ext4qcow2", "qcow2", "reflink")]
for n in sorted({r["vms"] for r in rows}, key=int):
    print(f"\n### {n} VM{'s' if n != '1' else ''} at once\n")
    print("| median | " + " | ".join(f"{d}, {c} cache" for d, c in cols) + " | reflink ÷ qcow2 on XFS, warm | cold |")
    print("|---" * (len(cols) + 3) + "|")
    for k in keys:
        m = {dc: statistics.median(vals[(n, k) + dc]) if vals[(n, k) + dc] else None for dc in cols}
        cells = [f"{m[dc]:,.0f} ({len(vals[(n, k) + dc])})" if m[dc] is not None else "-" for dc in cols]
        rel = []
        for c in ("warm", "cold"):
            q, x = m[("qcow2", c)], m[("reflink", c)]
            rel.append(f"{x / q:.2f}×" if q and x is not None else "-")
        print(f"| {k}{' ↑' if k in higher else ''} | " + " | ".join(cells) + " | " + " | ".join(rel) + " |")
print("\next4qcow2: qcow2 overlays on the runner's ext4 root partition, as fleetci does today. qcow2 and reflink: on the XFS.")
print("(n) is the number of samples: VMs × rounds. ↑ higher is better; the rest are times, lower is better.")
