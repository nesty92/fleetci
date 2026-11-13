#!/usr/bin/env python3
"""A merge train on a playground repo, as GitLab's merge trains and GitHub's
merge queue run one, with the queue played here: neither playground's plan
has one.

    drive.py <github|gitlab> <cache|plain> <run> [cars] [width]

Main first gets the train's base commit and its pipeline, which fills
main's caches. Then each car is a commit on the car ahead, pushed to
train/<run>/car-<n>: its pipeline runs on main plus the cars ahead, as a
train's does. Up to <width> cars run at once. A car merges, its commit
pushed to main, when it passes and the car ahead has merged; main's
pipeline then runs on it. A car that fails stops the train.

Each job gets a class its size, as a project would pick: the Go tests
4 vCPUs and 4 GB, the web build 2 vCPUs and 3 GB (its Next.js build runs
out of memory in 2 GB). In "cache" mode the classes have cache disks; in
"plain" mode (the classes ending in -nodisk) they don't, and the jobs'
caches come only from GitHub's cache or the runner's S3.
Each job's timings and cache lines go to <out>/<run>.jsonl.
"""
import json
import os
import re
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.expanduser("~/.local/share/fleetci-cachelab/train")
GO = ["275b04f9c267493d5ddcf292bcf8614fd9592e6e", "85032553f9e86429353f5f403ef950547e0893ca",
      "848fa7a628b40fd55d7f78827c107b48807087a4", "6b6d4987915aae62c295feba605eb5184351db8a",
      "fcff24533aff4a3d3a303819fff1cacb9450415c", "00bd7cd75a80af375e5b63ffdb333c8afd387b05"]
WEB = ["c2f8235e1f3ea903ad8b7f61447c4f74164b5c58", "e1e1075a451359f82e155d29ba74398d5c5e7f5c",
       "8d7ac18b472850ec04eab0b4060028b5a6923c6d", "8603c8ea35ded822811d3201812e717b22edbbab",
       "31b5487f62540d4b74f2d3114918c0ebe28f8a12", "a8989b522fa3672a2ae52f053105e40993b36ca1"]
GH = "WireZone/fleetci-playground"
GL = "Nesty92%2Ffleetci-playground"
CLASSES = {"go": "4vcpu-4gb", "web": "2vcpu-3gb"}
REMOTE = {"github": "git@github.com:WireZone/fleetci-playground.git",
          "gitlab": "https://gitlab.com/Nesty92/fleetci-playground.git"}
STAMP = re.compile(r"^20\d\d-\d\d-\d\dT[\d:.]+Z (\d\d[OE]\+? )?")
# GitLab over HTTPS with glab's login: the SSH key may be another account's.
CRED = ["-c", "credential.https://gitlab.com.helper=", "-c", "credential.https://gitlab.com.helper=!glab auth git-credential"]


def sh(*args, check=True):
    r = subprocess.run(args, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise SystemExit(f"{' '.join(args[:4])}: {r.stderr.strip()[:500]}")
    return r.stdout


def api(forge, path):
    if forge == "github":
        out = sh("gh", "api", path, check=False)
    else:
        out = sh("glab", "api", path, check=False)
    try:
        return json.loads(out)
    except ValueError:
        return None


def log(msg):
    print(time.strftime("%H:%M:%S"), msg, flush=True)


class Train:
    def __init__(self, forge, mode, run, cars, width):
        self.forge, self.mode, self.run, self.cars, self.width = forge, mode, run, cars, width
        self.dir = os.path.join(OUT, "work", f"{forge}-{run}")
        self.git = ["git", "-C", self.dir, "-c", "user.name=nesty92", "-c", "user.email=nesty92@users.noreply.github.com"] + CRED
        self.out = open(os.path.join(OUT, f"{run}.jsonl"), "a")
        self.t0 = time.time()

    def at(self):
        return round(time.time() - self.t0)

    def commit(self, car):
        with open(os.path.join(self.dir, "train.env"), "w") as f:
            f.write(f"RUN={self.run}\nCAR={car}\nGO_COMMIT={GO[car]}\nWEB_COMMIT={WEB[car]}\n")
        sh(*self.git, "add", "-A")
        sh(*self.git, "commit", "-q", "-m", f"train {self.run}: car {car}")
        return sh(*self.git, "rev-parse", "HEAD").strip()

    def push(self, sha, ref):
        sh(*self.git, "push", "-q", "-f", "origin", f"{sha}:refs/heads/{ref}")

    def cls(self, job):
        return CLASSES[job] + ("" if self.mode == "cache" else "-nodisk")

    def setup(self):
        os.makedirs(os.path.dirname(self.dir), exist_ok=True)
        sh("rm", "-rf", self.dir)
        sh("git", *CRED, "clone", "-q", "--depth", "1", REMOTE[self.forge], self.dir)
        os.makedirs(os.path.join(self.dir, "ci"), exist_ok=True)
        with open(os.path.join(HERE, "ci", "job.sh")) as f, open(os.path.join(self.dir, "ci", "job.sh"), "w") as g:
            g.write(f.read())
        if self.forge == "github":
            src, dst = "train.yml", ".github/workflows/train.yml"
            fill = {"@LABEL_GO@": "fleetci-" + self.cls("go"), "@LABEL_WEB@": "fleetci-" + self.cls("web"), "@RUN@": self.run}
        else:
            src, dst = "gitlab-ci.yml", ".gitlab-ci.yml"
            fill = {"@CLASS_GO@": self.cls("go"), "@CLASS_WEB@": self.cls("web"), "@RUN@": self.run}
        with open(os.path.join(HERE, src)) as f:
            s = f.read()
        for k, v in fill.items():
            s = s.replace(k, v)
        os.makedirs(os.path.dirname(os.path.join(self.dir, dst)), exist_ok=True)
        with open(os.path.join(self.dir, dst), "w") as f:
            f.write(s)

    # Pipelines: the run of a sha on a ref, and its jobs.
    def pipeline(self, sha, ref):
        if self.forge == "github":
            r = api("github", f"repos/{GH}/actions/runs?branch={ref}&head_sha={sha}&event=push")
            runs = [w for w in (r or {}).get("workflow_runs", []) if w["name"] == "train"]
            if not runs:
                return None
            w = runs[0]
            return {"id": w["id"], "done": w["status"] == "completed", "ok": w["conclusion"] == "success"}
        r = api("gitlab", f"projects/{GL}/pipelines?ref={ref}&sha={sha}")
        if not r:
            return None
        p = r[0]
        st = p["status"]
        return {"id": p["id"], "done": st in ("success", "failed", "canceled", "skipped"), "ok": st == "success"}

    def wait(self, sha, ref, deadline=3600):
        end = time.time() + deadline
        while time.time() < end:
            p = self.pipeline(sha, ref)
            if p and p["done"]:
                return p
            time.sleep(10)
        raise SystemExit(f"{ref} {sha[:8]}: no result in {deadline}s")

    def jobs(self, p, ref, car):
        """Each job's times and the lines that say what its caches did."""
        if self.forge == "github":
            js = api("github", f"repos/{GH}/actions/runs/{p['id']}/jobs").get("jobs", [])
        else:
            js = api("gitlab", f"projects/{GL}/pipelines/{p['id']}/jobs") or []
        for j in js:
            if self.forge == "github":
                text = sh("gh", "api", "--allow-escape-sequences", f"repos/{GH}/actions/jobs/{j['id']}/logs", check=False)
                rec = {"started": j["started_at"], "finished": j["completed_at"], "runner": j.get("runner_name")}
                created = api("github", f"repos/{GH}/actions/runs/{p['id']}")
                rec["queued_from"] = created and created.get("run_started_at")
            else:
                text = sh("glab", "api", f"projects/{GL}/jobs/{j['id']}/trace", check=False)
                rec = {"started": j["started_at"], "finished": j["finished_at"], "duration": j["duration"],
                       "queued": j["queued_duration"], "runner": (j.get("runner") or {}).get("description")}
            lines = []
            for line in text.splitlines():
                # GitHub's lines start with a timestamp, GitLab's with one
                # and a stream ("01O", "00E+").
                line = STAMP.sub("", line)
                if any(k in line for k in ("TRAIN ", "fleetci cache:", "Cache disk", "Cache restored", "Cache not found",
                                           "Cache saved", "Cache Size", "cache.zip", "Downloading cache", "Successfully extracted",
                                           "No URL provided", "Uploading cache", "Created cache", "Checking cache", "Pulling docker image")):
                    lines.append(line.strip()[:300])
            rec.update({"forge": self.forge, "mode": self.mode, "run": self.run, "car": car, "ref": ref,
                        "job": j["name"], "ok": (j.get("conclusion") or j.get("status")) == "success", "lines": lines})
            self.out.write(json.dumps(rec) + "\n")
            self.out.flush()
            train = [l for l in lines if l.startswith("TRAIN ")]
            log(f"  {ref} {j['name']}: {train[-1] if train else 'no TRAIN line'}")

    def go(self):
        self.setup()
        base = self.commit(0)
        self.push(base, "main")
        log(f"{self.forge} {self.mode} {self.run}: main at car 0, {base[:8]}")
        p = self.wait(base, "main")
        self.jobs(p, "main", 0)
        if not p["ok"]:
            raise SystemExit("main's base pipeline failed")
        self.t0 = time.time()
        log("train starts")
        running = []  # (car, sha, ref), oldest first
        merges = []
        nxt = 1
        while nxt <= self.cars or running:
            while len(running) < self.width and nxt <= self.cars:
                sha = self.commit(nxt)
                ref = f"train/{self.run}/car-{nxt}"
                self.push(sha, ref)
                log(f"car {nxt} starts at {self.at()}s, {sha[:8]}")
                running.append((nxt, sha, ref))
                nxt += 1
            car, sha, ref = running[0]
            p = self.pipeline(sha, ref)
            if not (p and p["done"]):
                time.sleep(10)
                continue
            self.jobs(p, ref, car)
            if not p["ok"]:
                raise SystemExit(f"car {car} failed at {self.at()}s: the train stops")
            self.push(sha, "main")
            merges.append((car, sha))
            running.pop(0)
            log(f"car {car} merged at {self.at()}s")
        log(f"train of {self.cars} cars, {self.width} at once: {self.at()}s")
        for car, sha in merges:
            p = self.wait(sha, "main")
            self.jobs(p, "main", car)
        log(f"main's pipelines done at {self.at()}s")


if __name__ == "__main__":
    forge, mode, run = sys.argv[1:4]
    cars = int(sys.argv[4]) if len(sys.argv) > 4 else 5
    width = int(sys.argv[5]) if len(sys.argv) > 5 else 2
    os.makedirs(OUT, exist_ok=True)
    Train(forge, mode, run, cars, width).go()
