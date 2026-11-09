#!/usr/bin/env python3
"""forge.py: the demo's jobs as the forges see them, every few seconds.

    forge.py <since, UTC ISO> <state.json>

Runs on the host, with gh and glab logged in. Prints the GitHub runs of
WireZone/fleetci-playground and the GitLab pipelines of the playground's
demo branch created since <since>, job by job, and writes how many jobs
there are and how many have finished to <state.json> for record.py.
"""
import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone

GH_REPO = 'WireZone/fleetci-playground'
GL_PROJECT = 'Nesty92%2Ffleetci-playground'
GL_REF = 'demo'

G, Y, R, D, B, X = '\033[32m', '\033[33m', '\033[31m', '\033[2m', '\033[1m', '\033[0m'


def api(cmd):
    try:
        return json.loads(subprocess.run(cmd, capture_output=True, text=True, timeout=20).stdout or 'null')
    except (subprocess.TimeoutExpired, json.JSONDecodeError):
        return None


def ts(s):
    return datetime.fromisoformat(s.replace('Z', '+00:00')) if s else None


def took(start, end):
    if not start:
        return ''
    s = int(((end or datetime.now(timezone.utc)) - start).total_seconds())
    return f'{s // 60}:{s % 60:02d}'


def mark(state):
    # (glyph, colour, finished, ok)
    return {
        'success': ('✓ passed ', G, True, True),
        'failed': ('✗ failed ', R, True, False), 'failure': ('✗ failed ', R, True, False),
        'cancelled': ('✗ cancelled', R, True, False), 'canceled': ('✗ cancelled', R, True, False),
        'skipped': ('– skipped', D, True, True), 'manual': ('– manual ', D, True, True),
        'in_progress': ('● running', Y, False, False), 'running': ('● running', Y, False, False),
    }.get(state, ('○ waiting', D, False, False))


def github(since):
    runs = api(['gh', 'api', f'repos/{GH_REPO}/actions/runs?created=>={since}&per_page=20']) or {}
    rows = []
    for run in sorted(runs.get('workflow_runs') or [], key=lambda r: r['created_at']):
        jobs = api(['gh', 'api', f'repos/{GH_REPO}/actions/runs/{run["id"]}/jobs']) or {}
        for j in jobs.get('jobs') or []:
            state = j['conclusion'] or j['status']
            rows.append((run['name'], j['name'], state, took(ts(j['started_at']), ts(j['completed_at'])),
                         ' '.join(l for l in j.get('labels') or [])))
    return rows


def gitlab(since):
    pipes = api(['glab', 'api', f'projects/{GL_PROJECT}/pipelines?ref={GL_REF}&updated_after={since}']) or []
    rows = []
    for p in sorted(pipes if isinstance(pipes, list) else [], key=lambda p: p['id']):
        if ts(p['created_at']) < ts(since):
            continue
        jobs = api(['glab', 'api', f'projects/{GL_PROJECT}/pipelines/{p["id"]}/jobs']) or []
        for j in sorted(jobs if isinstance(jobs, list) else [], key=lambda j: j['id']):
            rows.append((f'#{p["id"]}'[-6:], j['name'], j['status'], took(ts(j['started_at']), ts(j['finished_at'])),
                         ' '.join(j.get('tag_list') or [])))
    return rows


def show(title, rows, out):
    out.append(f'{B}{title}{X}')
    if not rows:
        out.append(f'  {D}no jobs yet{X}')
    for wf, name, state, t, labels in rows:
        glyph, c, _, _ = mark(state)
        out.append(f'  {wf[:10]:<10} {name[:12]:<12} {c}{glyph:<11}{X} {t:>5}  {D}{labels[:30]}{X}')


def main():
    since, path = sys.argv[1], sys.argv[2]
    while True:
        gh, gl = github(since), gitlab(since)
        out = []
        show(f'GitHub · {GH_REPO}', gh, out)
        show(f'GitLab · {GL_PROJECT.replace("%2F", "/")} @ {GL_REF}', gl, out)
        sys.stdout.write('\033[H\033[2J' + '\n'.join(out))
        sys.stdout.flush()
        state = {f: {'jobs': len(rows), 'done': sum(mark(r[2])[2] for r in rows), 'ok': sum(mark(r[2])[3] for r in rows)}
                 for f, rows in (('github', gh), ('gitlab', gl))}
        with open(path + '.tmp', 'w') as f:
            json.dump(state, f)
        os.replace(path + '.tmp', path)
        time.sleep(2)


if __name__ == '__main__':
    try:
        main()
    except KeyboardInterrupt:
        pass
