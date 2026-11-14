#!/usr/bin/env python3
"""A steady load of pull requests on a playground repo, measured as GitLab
and GitHub measure their runners: each job's time in the queue and the
time of each of its phases, at percentiles, not one end-to-end number.

    load.py <github|gitlab> <disk|plain|nocache> <run> [prs] [every]

Main first gets a base commit and its pipeline, which fills main's caches.
Then a pull request arrives every <every> seconds on average (default 150,
at random, as arrivals are: a Poisson process with a fixed seed), <prs> of
them (default 20). Each is a commit of the next of 40 real consecutive
commits of fleetci (the Go job: go vet, go test -race) and of
vercel/chatbot (the web job: pnpm install, checks, next build), pushed to
load/<run>/pr-<n>. When its pipeline passes it merges: its commit goes to
main, whose pipeline runs on it, unless a later one merged first. A failed
job or push is recorded and retried or skipped; it never stops the run.

Modes: "disk", the classes with cache disks; "plain", the same without
(the jobs' caches come from GitHub's cache or the runner's S3 bucket);
"nocache", without the jobs' own caches at all. Which L2 the GitHub jobs
use (GitHub's cache or the bucket) is the lab controller's config.

Each job's record goes to <out>/<run>.jsonl; the controller's metrics, at
the start and the end, to <run>.metrics-start and <run>.metrics-end (set
LOAD_METRICS to the command that prints them; the controller serves them
when [controller] metrics is set).
summary.py reads them.
"""
import datetime
import json
import os
import random
import re
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
TRAIN = os.path.join(HERE, "..", "train")
OUT = os.path.expanduser("~/.local/share/fleetci-cachelab/load")
GH = "WireZone/fleetci-playground"
GL = "Nesty92%2Ffleetci-playground"
REMOTE = {"github": "git@github.com:WireZone/fleetci-playground.git",
          "gitlab": "https://gitlab.com/Nesty92/fleetci-playground.git"}
CLASSES = {"go": "4vcpu-4gb", "web": "2vcpu-4gb"}
# GitLab over HTTPS with glab's login: the SSH key may be another account's.
CRED = ["-c", "credential.https://gitlab.com.helper=", "-c", "credential.https://gitlab.com.helper=!glab auth git-credential"]
METRICS = os.environ.get("LOAD_METRICS", "docker exec fleetci-cachelab wget -qO- http://127.0.0.1:9482/metrics").split()
STAMP = re.compile(r"^20\d\d-\d\d-\d\dT[\d:.]+Z (\d\d[OE]\+? )?")
SECTION = re.compile(r"section_(start|end):(\d+):([\w.-]+)")
KEEP = ("TRAIN ", "Cache disk", "Cache restored", "Cache not found", "Cache saved", "Cache Size",
        "Downloading cache", "Successfully extracted", "No URL provided", "Uploading cache", "Created cache",
        "Checking cache", "No cache disk")


def sh(*args, check=True, tries=1):
    for i in range(tries):
        r = subprocess.run(args, capture_output=True, text=True)
        if r.returncode == 0 or not check:
            return r.stdout
        log(f"{' '.join(args[:4])}: {r.stderr.strip()[:300]}" + (f"; again in {10 * (i + 1)}s" if i + 1 < tries else ""))
        time.sleep(10 * (i + 1))
    raise RuntimeError(f"{' '.join(args[:4])} failed {tries} times")


def api(forge, path):
    out = sh("gh" if forge == "github" else "glab", "api", path, check=False)
    try:
        return json.loads(out)
    except ValueError:
        return None


def log(msg):
    print(time.strftime("%H:%M:%S"), msg, flush=True)


def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp() if s else None


class Load:
    def __init__(self, forge, mode, run, prs, every):
        self.forge, self.mode, self.run, self.prs, self.every = forge, mode, run, prs, every
        self.dir = os.path.join(OUT, "work", f"{forge}-{run}")
        self.git = ["git", "-C", self.dir, "-c", "user.name=nesty92", "-c", "user.email=nesty92@users.noreply.github.com"] + CRED
        os.makedirs(OUT, exist_ok=True)
        self.out = open(os.path.join(OUT, f"{run}.jsonl"), "a")
        with open(os.path.join(HERE, "commits.json")) as f:
            self.commits = json.load(f)
        self.merged = -1

    def cls(self, job):
        return CLASSES[job] + ("" if self.mode == "disk" else "-nodisk")

    def setup(self):
        sh("rm", "-rf", self.dir)
        sh("git", *CRED, "clone", "-q", "--depth", "1", REMOTE[self.forge], self.dir, tries=3)
        # Only this run's pipeline: other workflows would run on main too.
        sh("rm", "-rf", os.path.join(self.dir, ".github", "workflows"))
        os.makedirs(os.path.join(self.dir, "ci"), exist_ok=True)
        with open(os.path.join(TRAIN, "ci", "job.sh")) as f, open(os.path.join(self.dir, "ci", "job.sh"), "w") as g:
            g.write(f.read())
        cache = self.mode != "nocache"
        if self.forge == "github":
            src, dst = "load.yml", ".github/workflows/load.yml"
            fill = {"@LABEL_GO@": "fleetci-" + self.cls("go"), "@LABEL_WEB@": "fleetci-" + self.cls("web"),
                    "@CACHE@": "true" if cache else "false"}
        else:
            src, dst = "gitlab-ci.yml", ".gitlab-ci.yml"
            fill = {"@CLASS_GO@": self.cls("go"), "@CLASS_WEB@": self.cls("web"),
                    "@POLICY@": "pull-push" if cache else "pull", "@CACHE_KEY@": ("load-" if cache else "load-none-") + self.run}
        fill["@RUN@"] = self.run
        with open(os.path.join(HERE, src)) as f:
            s = f.read()
        for k, v in fill.items():
            s = s.replace(k, v)
        os.makedirs(os.path.dirname(os.path.join(self.dir, dst)), exist_ok=True)
        with open(os.path.join(self.dir, dst), "w") as f:
            f.write(s)

    def commit(self, n):
        go, web = self.commits["go"]["commits"], self.commits["web"]["commits"]
        with open(os.path.join(self.dir, "train.env"), "w") as f:
            f.write(f"RUN={self.run}\nCAR={n}\nGO_COMMIT={go[n % len(go)]}\nWEB_COMMIT={web[n % len(web)]}\n")
        sh(*self.git, "add", "-A")
        sh(*self.git, "commit", "-q", "-m", f"load {self.run}: pr {n}")
        return sh(*self.git, "rev-parse", "HEAD").strip()

    def push(self, sha, ref):
        sh(*self.git, "push", "-q", "-f", "origin", f"{sha}:refs/heads/{ref}", tries=4)

    def pipeline(self, sha, ref):
        if self.forge == "github":
            r = api("github", f"repos/{GH}/actions/runs?branch={ref}&head_sha={sha}&event=push")
            runs = [w for w in (r or {}).get("workflow_runs", []) if w["name"] == "load"]
            if not runs:
                return None
            w = runs[0]
            return {"id": w["id"], "done": w["status"] == "completed", "ok": w["conclusion"] == "success"}
        r = api("gitlab", f"projects/{GL}/pipelines?ref={ref}&sha={sha}")
        if not r:
            return None
        p = r[0]
        return {"id": p["id"], "done": p["status"] in ("success", "failed", "canceled", "skipped"), "ok": p["status"] == "success"}

    def record(self, p, kind, n, ref):
        """Each job's queue time and phases, and the lines that say what
        its caches did."""
        if self.forge == "github":
            js = (api("github", f"repos/{GH}/actions/runs/{p['id']}/jobs") or {}).get("jobs", [])
        else:
            js = api("gitlab", f"projects/{GL}/pipelines/{p['id']}/jobs") or []
        for j in js:
            phases = {}
            if self.forge == "github":
                text = sh("gh", "api", "--allow-escape-sequences", f"repos/{GH}/actions/jobs/{j['id']}/logs", check=False).lstrip("\ufeff")
                created, started, ended = ts(j.get("created_at")), ts(j.get("started_at")), ts(j.get("completed_at"))
                for st in j.get("steps") or []:
                    a, b = ts(st.get("started_at")), ts(st.get("completed_at"))
                    if a and b:
                        phases[st["name"]] = phases.get(st["name"], 0) + b - a
                runner, ok = j.get("runner_name"), j.get("conclusion") == "success"
            else:
                text = sh("glab", "api", f"projects/{GL}/jobs/{j['id']}/trace", check=False)
                created, started, ended = ts(j.get("created_at")), ts(j.get("started_at")), ts(j.get("finished_at"))
                opened = {}
                for kind_, at, name in SECTION.findall(text):
                    if kind_ == "start":
                        opened[name] = int(at)
                    elif name in opened:
                        phases[name] = phases.get(name, 0) + int(at) - opened.pop(name)
                runner, ok = (j.get("runner") or {}).get("description"), j.get("status") == "success"
            lines = []
            for line in text.splitlines():
                line = re.sub(r"\x1b\[[0-9;]*[A-Za-z]|section_(start|end):\d+:[\w.-]+(\[[^\]]*\])?\r?", "", STAMP.sub("", line))
                if any(k in line for k in KEEP):
                    lines.append(line.strip()[:300])
            rec = {"forge": self.forge, "mode": self.mode, "run": self.run, "kind": kind, "n": n, "ref": ref,
                   "job": j["name"], "ok": ok, "runner": runner, "created": created, "started": started, "ended": ended,
                   "queued": started - created if created and started else None,
                   "wall": ended - started if started and ended else None, "phases": phases, "lines": lines}
            self.out.write(json.dumps(rec) + "\n")
            self.out.flush()
            train = [l for l in lines if l.startswith("TRAIN ")]
            q = f"{rec['queued']:.0f}s" if rec["queued"] is not None else "?"
            log(f"  {kind} {n} {j['name']}: queued {q}, {train[-1][:150] if train else 'no TRAIN line'}")

    def metrics(self, name):
        r = subprocess.run(METRICS, capture_output=True, text=True)
        with open(os.path.join(OUT, f"{self.run}.{name}"), "w") as f:
            f.write(r.stdout)

    def go(self):
        self.setup()
        base = self.commit(0)
        self.push(base, "main")
        log(f"{self.forge} {self.mode} {self.run}: main's base {base[:8]}")
        tracked = [("base", 0, base, "main")]
        while tracked:  # the base pipeline, before the load starts
            p = self.pipeline(base, "main")
            if p and p["done"]:
                self.record(p, "base", 0, "main")
                tracked = []
            else:
                time.sleep(15)
        self.metrics("metrics-start")
        rng = random.Random(self.run)
        t, at = time.time(), []
        for _ in range(self.prs):
            t += rng.expovariate(1 / self.every)
            at.append(t)
        log(f"load starts: {self.prs} pull requests over {at[-1] - time.time():.0f}s")
        n, deadline = 0, at[-1] + 3600
        while (n < self.prs or tracked) and time.time() < deadline:
            while n < self.prs and time.time() >= at[n]:
                n += 1
                sha, ref = self.commit(n), f"load/{self.run}/pr-{n}"
                try:
                    self.push(sha, ref)
                    tracked.append(("pr", n, sha, ref))
                    log(f"pr {n} pushed, {sha[:8]}")
                except RuntimeError as e:
                    log(f"pr {n} not pushed: {e}")
            for item in list(tracked):
                kind, i, sha, ref = item
                p = self.pipeline(sha, ref)
                if not (p and p["done"]):
                    continue
                tracked.remove(item)
                self.record(p, kind, i, ref)
                if kind != "pr":
                    continue
                if not p["ok"]:
                    log(f"pr {i} failed: not merged")
                elif i < self.merged:
                    log(f"pr {i} passed after pr {self.merged} merged: not merged")
                else:
                    try:
                        self.push(sha, "main")
                        self.merged = i
                        tracked.append(("main", i, sha, "main"))
                        log(f"pr {i} merged")
                    except RuntimeError as e:
                        log(f"pr {i} not merged: {e}")
            time.sleep(15)
        self.metrics("metrics-end")
        log(f"done; {len(tracked)} pipelines left unrecorded" if tracked else "done")


if __name__ == "__main__":
    forge, mode, run = sys.argv[1:4]
    if mode not in ("disk", "plain", "nocache"):
        raise SystemExit("mode: disk, plain or nocache")
    prs = int(sys.argv[4]) if len(sys.argv) > 4 else 20
    every = float(sys.argv[5]) if len(sys.argv) > 5 else 150
    Load(forge, mode, run, prs, every).go()
