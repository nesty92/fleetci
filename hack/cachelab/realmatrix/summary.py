"""Sums up a forge's results (results/<forge>.txt, the jobs' end lines):
per language, cold is the mean of the two jobs with no cache disk, warm the
median of the jobs with it after the first two, which fill it.

    python3 -I summary.py results/github.txt
"""
import re
import statistics as st
import sys

for path in sys.argv[1:]:
    rows = {}
    for line in open(path, errors="replace"):
        m = re.search(r" end (\S+) (\S+) \S+ disk=(\S+) (.*)", line)
        if not m:
            continue
        _, lang, disk, rest = m.groups()
        rows.setdefault(lang, []).append((disk, dict(re.findall(r"(\w+)=([\d.]+)", rest))))
    print(path)
    for lang, rs in rows.items():
        cold = st.mean(float(k["total"]) for d, k in rs if d == "no")
        warm = [k for d, k in rs if d == "yes"][2:]
        wt = st.median(float(k["total"]) for k in warm)
        queued = [float(k["queued"]) for _, k in rs if "queued" in k]
        pull = ""
        if all("pull" in k for _, k in rs):
            pull = " pull %.1f -> %.1f s" % (
                st.mean(float(k["pull"]) for d, k in rs if d == "no"),
                st.median(float(k["pull"]) for k in warm),
            )
        print(f"{lang:11} cold {cold:6.1f} s  warm {wt:6.1f} s  saved {100 * (1 - wt / cold):3.0f}%"
              f"  queued {min(queued):.0f}-{max(queued):.0f} s{pull}")
