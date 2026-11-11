"""Turns matrix.sh's log into docs/measurements.md's table.

python3 -I hack/cachelab/table.py <matrix log>

Per language: the cold job on the first commit, the cached jobs after the
first (each on the layer of the one before), and the cold job on the last
commit. Times are pull plus the project's steps, without boot and
shutdown, which a cache doesn't change.
"""
import re
import statistics
import sys

job = re.compile(r"^(\S+) (\w+) job (\d+) \(cache (yes|no)\): (.*?), layer (\S+) MB$")
size = re.compile(r"^(\S+): cache disk (\d+) MB in (\d+) layers$")
skip = {"boot", "mount", "shutdown"}

runs, sizes, order = {}, {}, []
for line in open(sys.argv[1]):
    line = line.strip()
    if m := size.match(line):
        sizes[m[1]] = (int(m[2]), int(m[3]))
        continue
    m = job.match(line)
    if not m:
        continue
    name, n, cache = m[1], int(m[3]), m[4] == "yes"
    steps, failed = {}, False
    for part in m[5].split(", "):
        k, v = part.split(" ", 1)
        if v.endswith("FAILED"):
            failed, v = True, v[: -len("FAILED")]
        steps[k] = float(v.rstrip("s"))
    if name not in runs:
        runs[name] = []
        order.append(name)
    runs[name].append((n, cache, steps, failed))


def work(steps):
    return sum(v for k, v in steps.items() if k not in skip)


def fmt(s):
    return f"{s:.0f} s" if s >= 10 else f"{s:.1f} s"


print("| Language | Cold, first commit | Cached, median | Cold, last commit | Saved | Pull cold → cached | Cache disk |")
print("|---|---|---|---|---|---|---|")
for name in order:
    r = runs[name]
    cold = [x for x in r if not x[1]]
    warm = [x for x in r if x[1] and x[0] > 1]  # job 1 fills the empty disk
    if len(cold) < 1 or not warm:
        continue
    first, last = cold[0], cold[-1]
    med = statistics.median(work(x[2]) for x in warm)
    pull = statistics.median(x[2].get("pull", 0) for x in warm)
    # A failed run stops early, so it doesn't count as a baseline.
    base = statistics.mean(work(x[2]) for x in (first, last) if not x[3]) if not (first[3] and last[3]) else work(last[2])
    mb, layers = sizes.get(name, (0, 0))
    note = " (some steps failed)" if any(x[3] for x in r) else ""
    print(f"| {name}{note} | {fmt(work(first[2]))} | {fmt(med)} | {fmt(work(last[2]))} | {100 * (1 - med / base):.0f}% "
          f"| {fmt(first[2].get('pull', 0))} → {fmt(pull)} | {mb / 1024:.1f} GB in {layers} layers |")

print()
print("Per step, cold (last commit) → cached (median):")
print()
for name in order:
    r = runs[name]
    cold = [x for x in r if not x[1]]
    warm = [x for x in r if x[1] and x[0] > 1]
    if not cold or not warm:
        continue
    parts = []
    for k in cold[-1][2]:
        if k in skip:
            continue
        w = statistics.median(x[2].get(k, 0) for x in warm)
        parts.append(f"{k} {fmt(cold[-1][2][k])} → {fmt(w)}")
    print(f"- **{name}**: " + ", ".join(parts))
