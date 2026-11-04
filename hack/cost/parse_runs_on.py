# Streams workflows.tar.gz from stdin; for each wanted workflow file, writes
# one JSON line per job: file hash, job key, job name, runs-on, and the
# values of any matrix key runs-on refers to.
import json, re, sys, tarfile, yaml
want = set(open(sys.argv[1]).read().split())
out = open(sys.argv[2], "w")
ref = re.compile(r"\$\{\{\s*matrix\.([A-Za-z0-9_-]+)")
seen = bad = 0
class L(yaml.SafeLoader): pass
L.yaml_implicit_resolvers = {k: [r for r in v if r[0] != "tag:yaml.org,2002:bool"] for k, v in yaml.SafeLoader.yaml_implicit_resolvers.items()}
with tarfile.open(fileobj=sys.stdin.buffer, mode="r|gz") as t:
    for m in t:
        h = m.name.rsplit("/", 1)[-1]
        if not m.isfile() or h not in want: continue
        seen += 1
        try:
            doc = yaml.load(t.extractfile(m).read(), Loader=L)
            jobs = doc.get("jobs") or {}
        except Exception:
            bad += 1; continue
        if not isinstance(jobs, dict): continue
        for key, j in jobs.items():
            if not isinstance(j, dict): continue
            ro = j.get("runs-on")
            if ro is None and "uses" in j: ro = "__reusable__"
            mvals = None
            s = json.dumps(ro)
            mm = ref.search(s)
            if mm:
                mat = ((j.get("strategy") or {}).get("matrix") or {}) if isinstance(j.get("strategy"), dict) else {}
                k = mm.group(1); vals = []
                if isinstance(mat, dict):
                    v = mat.get(k)
                    if isinstance(v, list): vals += v
                    for inc in mat.get("include") or []:
                        if isinstance(inc, dict) and k in inc: vals.append(inc[k])
                mvals = vals
            out.write(json.dumps({"h": h, "key": str(key), "name": j.get("name") if isinstance(j.get("name"), str) else None, "runs_on": ro, "matrix": mvals}, default=str) + "\n")
print(f"files {seen} unparsable {bad}", file=sys.stderr)
