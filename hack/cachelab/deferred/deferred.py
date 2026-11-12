#!/usr/bin/env python3
"""The host side of deferred.yml: takes the job's entries, waits for the
job to end, then uploads and finalizes each entry its delay later, and
says what GitHub answered and whether the entry is listed.

    deferred.py <listen address> <owner/repo>

The job's token stays in memory and is never printed.
"""
import base64
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

ADDR, REPO = sys.argv[1], sys.argv[2]
GOT = {}


def log(*a):
    print(time.strftime("%H:%M:%S"), *a, flush=True)


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/deferred" or not self.client_address[0].startswith("10.231.29."):
            self.send_response(404)
            self.end_headers()
            return
        GOT.update(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
        self.send_response(200)
        self.end_headers()

    def log_message(self, *a):
        pass


def call(url, data, headers, method="POST"):
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read()[:300]
    except urllib.error.HTTPError as e:
        return e.code, e.read()[:300]


def gh(path):
    return json.loads(subprocess.run(["gh", "api", path], capture_output=True, text=True).stdout or "null")


host, port = ADDR.rsplit(":", 1)
srv = HTTPServer((host, int(port)), H)
srv.timeout = 5
log("waiting for the job on", ADDR)
end = time.time() + 3 * 3600
while not GOT and time.time() < end:
    srv.handle_request()
srv.server_close()
if not GOT:
    sys.exit("no job came")
token = GOT.pop("token")
claims = json.loads(base64.urlsafe_b64decode(token.split(".")[1] + "=="))
log("token: valid for", claims.get("exp", 0) - int(time.time()), "s more")
run = GOT["entries"][0]["key"].split("-")[1]

# The job's end, as GitHub says it.
while True:
    jobs = (gh(f"repos/{REPO}/actions/runs/{run}/jobs") or {}).get("jobs", [])
    if jobs and all(j["status"] == "completed" for j in jobs):
        break
    time.sleep(5)
done = time.time()
log("job completed:", jobs[0]["conclusion"], jobs[0]["completed_at"])

svc = GOT["results"].rstrip("/") + "/twirp/github.actions.results.api.v1.CacheService/"
auth = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}
for e in sorted(GOT["entries"], key=lambda e: e["delay"]):
    while time.time() < done + e["delay"]:
        time.sleep(1)
    blob = os.urandom(4096)
    st, body = call(e["url"], blob, {"x-ms-blob-type": "BlockBlob", "Content-Type": "application/octet-stream"}, "PUT")
    log(f"{e['key']}: {e['delay']}s after the job, upload: {st} {body[:120]!r}")
    st, body = call(svc + "FinalizeCacheEntryUpload",
                    json.dumps({"key": e["key"], "version": e["version"], "size_bytes": str(len(blob))}).encode(), auth)
    log(f"{e['key']}: finalize: {st} {body[:200]!r}")
    time.sleep(5)
    listed = gh(f"repos/{REPO}/actions/caches?key={e['key']}") or {}
    log(f"{e['key']}: listed: {[(c['key'], c['size_in_bytes']) for c in listed.get('actions_caches', [])]}")
