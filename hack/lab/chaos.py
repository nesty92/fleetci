#!/usr/bin/env python3
"""chaos.py: faults on the lab's fleet while it runs jobs, at random, and
replayable.

    python3 -I hack/lab/chaos.py [--seed N | --plan plan.json] [--minutes M]
                                 [--faults kind,kind] [--dry-run] <out dir>

Runs on the host, with gh and glab logged in, against the lab after
`up.sh`. A seed makes the whole plan up front: when the jobs are started,
and each fault, its target and how long it lasts. The plan is saved in
<out dir>/plan.json, and `--seed N` or `--plan plan.json` runs the same one
again. What a fault hits at the time, such as which VM a vm-kill picks, is
picked by a number in the plan from what is there then, in a fixed order:
the same if the fleet is in the same state, which it may not be.

After the last fault everything is healed, and the run waits for the
workers to be Ready and every job to end, then checks:

  - the three workers are Ready again;
  - every job the run started ended;
  - no worker used more than its budget, in any sample;
  - no VM is left on a worker once its jobs are over;
  - every failed job had a fault around it.

<out dir> gets the plan, what was done when (events.jsonl), the fleet's
status every few seconds (status.jsonl), the jobs, the logs of the
controller and the workers, and report.md.
"""
import argparse
import json
import os
import random
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone

LAB = 'fleetci-lab'
GH_REPO = 'WireZone/fleetci-playground'
GL_REPO = 'Nesty92/fleetci-playground'
GL_PROJECT = GL_REPO.replace('/', '%2F')
GL_REF = 'demo'
WORKERS = ('eu', 'us', 'ap')
NET = {'ctl': '10.20.0.0/24', 'eu': '10.21.0.0/24', 'us': '10.22.0.0/24', 'ap': '10.23.0.0/24'}
DELAY = {'eu': 20, 'us': 80, 'ap': 160}  # machines.sh's
TAG = 'lab-chaos'

# kind: (weight, target, seconds it lasts or None for at once, what it is)
KINDS = {
    'vm-kill': (3, 'vm', None, "kill -9 a microVM's cloud-hypervisor, job or not"),
    'worker-kill': (2, 'worker', None, 'kill -9 the worker; systemd starts it again'),
    'worker-stop': (2, 'worker', (10, 60), 'SIGSTOP the worker, then SIGCONT'),
    'worker-cut': (2, 'worker', (15, 90), 'cut the worker from the controller, not from the forges'),
    'worker-offline': (1, 'worker', (15, 90), 'cut the worker from everything'),
    'lag': (2, 'worker', (20, 90), "add delay and loss to the worker's network"),
    'ctl-kill': (1, 'ctl', None, 'kill -9 the controller; systemd starts it again'),
    'ctl-stop': (1, 'ctl', (10, 40), 'SIGSTOP the controller, then SIGCONT'),
}
AFTERMATH = 90  # how long after an instant fault a job's failure is put down to it


def make_plan(seed, minutes, kinds):
    rng = random.Random(seed)
    end = minutes * 60
    events = []
    t = 0.0
    while t < end - 60:  # jobs, a round at a time, none in the last minute
        events.append({'at': round(t, 1), 'kind': 'jobs'})
        t += rng.uniform(90, 180)
    busy = {}  # target → until when a fault holds it
    t = 30.0  # the first jobs have started
    weights = [KINDS[k][0] for k in kinds]
    while True:
        t += rng.uniform(15, 45)
        if t >= end:
            break
        kind = rng.choices(kinds, weights)[0]
        _, target, span, _ = KINDS[kind]
        e = {'at': round(t, 1), 'kind': kind}
        if target == 'worker':
            e['target'] = rng.choice(WORKERS)
        elif target == 'ctl':
            e['target'] = 'ctl'
        else:
            e['pick'] = round(rng.random(), 4)
        if span:
            e['for'] = rng.randint(*span)
        if kind == 'lag':
            e['delay_ms'], e['loss_pct'] = rng.choice([300, 800, 2000]), rng.choice([0, 5, 20])
        key = e.get('target', 'vm')
        if busy.get(key, 0) > t:
            continue  # one fault at a time on each target
        busy[key] = t + e.get('for', 0)
        events.append(e)
    events.sort(key=lambda e: e['at'])
    return {'version': 1, 'seed': seed, 'minutes': minutes, 'faults': kinds, 'events': events}


# The lab.

def run(cmd, timeout=30):
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(cmd, 124, '', 'timed out')


def ssh(machine, cmd, timeout=30):
    return run(['docker', 'exec', LAB, '/data/ssh.sh', machine, cmd], timeout)


def box(cmd):
    """Runs cmd in the lab container, where the machines' networks are."""
    return run(['docker', 'exec', LAB, 'sh', '-c', cmd])


def status():
    r = ssh('ctl', 'fleetci fleet status -json', timeout=10)
    try:
        return json.loads(r.stdout) if r.returncode == 0 else None
    except json.JSONDecodeError:
        return None


def cut_rules(m, everything):
    """The iptables rules that cut m's network: from the controller's, or
    from all."""
    if everything:
        return [f'-s {NET[m]}', f'-d {NET[m]}']
    return [f'-s {NET[m]} -d {NET["ctl"]}', f'-s {NET["ctl"]} -d {NET[m]}']


def iptables(op, rule):
    return box(f'iptables {op} FORWARD {rule} -m comment --comment {TAG} -j DROP')


def netem(m, delay, jitter=0, loss=0):
    extra = (f' {jitter}ms' if jitter else '') + (f' loss {loss}%' if loss else '')
    return box(f'tc qdisc replace dev br-{m} root netem delay {delay}ms{extra}')


def vms():
    """Every microVM's (worker, pid), in a fixed order."""
    out = []
    for m in WORKERS:
        r = ssh(m, 'pidof cloud-hypervisor || true', timeout=10)
        out += [(m, int(p)) for p in r.stdout.split()]
    return sorted(out)


def unit(m):
    return 'fleetci-controller' if m == 'ctl' else 'fleetci-worker'


def signal(m, sig):
    # The main process only: a worker's VMs are in its unit too.
    return ssh(m, f'systemctl kill --kill-whom=main -s {sig} {unit(m)}')


def start(e):
    """Injects e's fault; answers what it did, in words."""
    k, m = e['kind'], e.get('target')
    if k == 'jobs':
        return start_jobs()
    if k == 'vm-kill':
        all_ = vms()
        if not all_:
            return 'no microVM to kill'
        w, pid = all_[min(int(e['pick'] * len(all_)), len(all_) - 1)]
        r = ssh(w, f'kill -9 {pid}')
        return f'killed microVM pid {pid} on {w} ({len(all_)} up)' + ('' if r.returncode == 0 else f': {r.stderr.strip()}')
    if k in ('worker-kill', 'ctl-kill'):
        r = signal(m, 'KILL')
        return f'killed {unit(m)} on {m}' + ('' if r.returncode == 0 else f': {r.stderr.strip()}')
    if k in ('worker-stop', 'ctl-stop'):
        signal(m, 'STOP')
        return f'stopped {unit(m)} on {m} for {e["for"]}s'
    if k in ('worker-cut', 'worker-offline'):
        for rule in cut_rules(m, k == 'worker-offline'):
            iptables('-I', rule)
        return f'cut {m} from {"everything" if k == "worker-offline" else "the controller"} for {e["for"]}s'
    if k == 'lag':
        netem(m, DELAY[m] + e['delay_ms'], e['delay_ms'] // 4, e['loss_pct'])
        return f'{m}: +{e["delay_ms"]}ms, {e["loss_pct"]}% loss for {e["for"]}s'
    raise ValueError(k)


def heal(e):
    k, m = e['kind'], e.get('target')
    if k.endswith('-stop'):
        signal(m, 'CONT')
        return f'resumed {unit(m)} on {m}'
    if k in ('worker-cut', 'worker-offline'):
        for rule in cut_rules(m, k == 'worker-offline'):
            iptables('-D', rule)
        return f'{m} reconnected'
    if k == 'lag':
        netem(m, DELAY[m])
        return f'{m}: back to {DELAY[m]}ms'
    return ''


def heal_all():
    """Whatever a fault left, gone: an interrupted run's too."""
    for m in ('ctl',) + WORKERS:
        signal(m, 'CONT')
    while box(f'iptables -S FORWARD | grep -q {TAG}').returncode == 0:
        rule = box(f'iptables -S FORWARD | grep {TAG} | head -1').stdout.strip()
        box(f'iptables {rule.replace("-A FORWARD", "-D FORWARD", 1)}')
    for m in WORKERS:
        netem(m, DELAY[m])


def start_jobs():
    done = []
    for cmd in (['gh', 'workflow', 'run', 'burst.yml', '-R', GH_REPO],
                ['gh', 'workflow', 'run', 'basic.yml', '-R', GH_REPO],
                ['glab', 'ci', 'run', '-R', GL_REPO, '-b', GL_REF]):
        r = run(cmd)
        done.append(' '.join(cmd[:3]) + ('' if r.returncode == 0 else f' FAILED: {r.stderr.strip()[:120]}'))
    return 'started jobs: ' + '; '.join(done)


# The forges' view.

def api(cmd):
    r = run(cmd, timeout=30)
    try:
        return json.loads(r.stdout or 'null')
    except json.JSONDecodeError:
        return None


def jobs(since):
    """Every job of the runs and pipelines started since since."""
    out = []
    runs = api(['gh', 'api', f'repos/{GH_REPO}/actions/runs?created=>={since}&per_page=100']) or {}
    for r in runs.get('workflow_runs') or []:
        for j in (api(['gh', 'api', f'repos/{GH_REPO}/actions/runs/{r["id"]}/jobs']) or {}).get('jobs') or []:
            out.append({'forge': 'github', 'id': j['id'], 'name': f'{r["name"]}/{j["name"]}',
                        'state': j['conclusion'] or j['status'], 'done': j['status'] == 'completed',
                        'started': j['started_at'], 'ended': j['completed_at'], 'runner': j.get('runner_name')})
    pipes = api(['glab', 'api', f'projects/{GL_PROJECT}/pipelines?ref={GL_REF}&updated_after={since}&per_page=100']) or []
    for p in pipes if isinstance(pipes, list) else []:
        if p['created_at'] < since:
            continue
        for j in api(['glab', 'api', f'projects/{GL_PROJECT}/pipelines/{p["id"]}/jobs']) or []:
            out.append({'forge': 'gitlab', 'id': j['id'], 'name': f'#{p["id"]}/{j["name"]}', 'state': j['status'],
                        'done': j['status'] in ('success', 'failed', 'canceled', 'skipped'),
                        'started': j['started_at'], 'ended': j['finished_at'], 'runner': (j.get('runner') or {}).get('description')})
    return sorted(out, key=lambda j: (j['started'] or '', j['name']))


# The run.

class Run:
    def __init__(self, out):
        self.out = out
        self.t0 = time.time()
        self.lock = threading.Lock()
        self.stop = threading.Event()
        self.over_budget = []

    def log(self, what, **extra):
        e = {'t': round(time.time() - self.t0, 1), 'at': now_iso(), 'what': what, **extra}
        with self.lock, open(os.path.join(self.out, 'events.jsonl'), 'a') as f:
            f.write(json.dumps(e) + '\n')
        print(f'{e["t"]:>7.1f}  {what}', flush=True)

    def sample(self):
        """The fleet every few seconds, and any worker past its budget."""
        with open(os.path.join(self.out, 'status.jsonl'), 'a') as f:
            while not self.stop.wait(3):
                s = status()
                row = {'t': round(time.time() - self.t0, 1), 'leader': (s or {}).get('leader'), 'workers': {}}
                for w in (s or {}).get('workers') or []:
                    rep, facts = w.get('report') or {}, w.get('facts') or {}
                    row['workers'][w['id']] = {'state': w['state'], 'vms': len(rep.get('vms') or []),
                                               'cpus': rep.get('used_cpus', 0), 'mem': rep.get('used_memory_mb', 0)}
                    if rep.get('used_cpus', 0) > facts.get('cpus', 0) or rep.get('used_memory_mb', 0) > facts.get('memory_mb', 0):
                        self.over_budget.append((row['t'], w['id'], rep.get('used_cpus'), facts.get('cpus')))
                f.write(json.dumps(row) + '\n')
                f.flush()


def now_iso():
    return datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')


def wait_for(what, cond, timeout, every=5):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(every)
    return None


def windows(plan, t0):
    """Each fault's span, in epoch seconds, with its aftermath."""
    out = []
    for e in plan['events']:
        if e['kind'] == 'jobs':
            continue
        a = t0 + e['at']
        out.append((a, a + e.get('for', 0) + AFTERMATH, e))
    return out


def ts(s):
    return datetime.fromisoformat(s.replace('Z', '+00:00')).timestamp() if s else None


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('out')
    ap.add_argument('--seed', type=int)
    ap.add_argument('--plan')
    ap.add_argument('--minutes', type=float, default=10)
    ap.add_argument('--faults', default=','.join(KINDS), help='the kinds of fault to pick from')
    ap.add_argument('--dry-run', action='store_true', help='print the plan, and do nothing')
    a = ap.parse_args()

    if a.plan:
        with open(a.plan) as f:
            plan = json.load(f)
    else:
        kinds = [k for k in a.faults.split(',') if k]
        if bad := [k for k in kinds if k not in KINDS]:
            sys.exit(f'no such fault: {", ".join(bad)}; there are {", ".join(KINDS)}')
        seed = a.seed if a.seed is not None else int.from_bytes(os.urandom(4), 'big')
        plan = make_plan(seed, a.minutes, kinds)
    replay = f'python3 -I hack/lab/chaos.py --seed {plan["seed"]} --minutes {plan["minutes"]:g}' + (
        '' if plan['faults'] == list(KINDS) else f' --faults {",".join(plan["faults"])}')
    for e in plan['events']:
        print(f'{e["at"]:>7.1f}  {e["kind"]:<14} ' + ' '.join(f'{k}={v}' for k, v in e.items() if k not in ('at', 'kind')))
    print(f'replay: {replay} <out dir>')
    if a.dry_run:
        return

    out = os.path.abspath(a.out)
    os.makedirs(out, exist_ok=False)
    with open(os.path.join(out, 'plan.json'), 'w') as f:
        json.dump(plan, f, indent=1)
    s = status()
    ready = [w['id'] for w in (s or {}).get('workers') or [] if w['state'] == 'Ready']
    if not s or s.get('leader') != 'ctl' or len(ready) < 3:
        sys.exit('the lab is not up: run docker exec fleetci-lab /lab/up.sh first')

    r = Run(out)
    since = now_iso()
    threading.Thread(target=r.sample, daemon=True).start()
    r.log('start', seed=plan['seed'], replay=replay)
    # Each fault's start, and its heal when it lasts, in time order.
    steps = []
    for i, e in enumerate(plan['events']):
        steps.append((e['at'], i, 0, e))
        if 'for' in e:
            steps.append((e['at'] + e['for'], i, 1, e))
    steps.sort(key=lambda s: s[:3])
    try:
        for at, _, healing, e in steps:
            time.sleep(max(0, r.t0 + at - time.time()))
            what = heal(e) if healing else start(e)
            r.log(what, kind=e['kind'], heal=bool(healing))
    finally:
        heal_all()
        r.log('calm: everything healed')

    # How the fleet comes out of it.
    findings = []
    ok = wait_for('three Ready workers', lambda: sum(w['state'] == 'Ready' for w in (status() or {}).get('workers') or []) >= 3,
                  600)
    r.log('three workers Ready' if ok else 'workers not Ready 10 minutes after the calm')
    if not ok:
        findings.append('the workers were not all Ready 10 minutes after the calm')
    js = wait_for('every job over', lambda: (j := jobs(since)) and all(x['done'] for x in j) and j, 45 * 60, every=20)
    if not js:
        js = jobs(since)
        findings.append(f'{sum(not x["done"] for x in js)} jobs had not ended 45 minutes after the calm')
    r.log(f'{len(js)} jobs, {sum(x["done"] for x in js)} over')
    time.sleep(30)  # the workers remove the last VMs
    left, idle, unknown = {}, {}, {}
    reports = {w['id']: w.get('report') or {} for w in (status() or {}).get('workers') or []}
    for m, rep in reports.items():
        if n := len(rep.get('vms') or []):
            left[m] = n
        if n := sum((rep.get('idle') or {}).values()):
            idle[m] = n
    for m in WORKERS:
        rep = reports.get(m, {})
        n = len(ssh(m, 'pidof cloud-hypervisor || true').stdout.split())
        if n > len(rep.get('vms') or []) + sum((rep.get('idle') or {}).values()):
            unknown[m] = n
    if left:
        findings.append(f'VMs left once the jobs were over: {left}')
    if unknown:
        findings.append(f'cloud-hypervisor processes no report accounts for: {unknown}')
    # Spares taskscaler booted for a wait that ended first: the worker
    # counts their room, and removes them after a minute idle.
    notes = [f'idle VMs, counted in the reports: {idle}'] if idle else []
    r.stop.set()
    if r.over_budget:
        findings.append(f'past the budget: {r.over_budget[:5]}')

    faults = windows(plan, r.t0)
    failed = [j for j in js if j['state'] in ('failure', 'failed', 'cancelled', 'canceled')]
    unexplained = []
    for j in failed:
        a, b = ts(j['started']) or ts(j['ended']) or 0, ts(j['ended']) or time.time()
        near = [e for fa, fb, e in faults if fa <= b and a <= fb]
        j['faults'] = [f'{e["kind"]}@{e["at"]}' for e in near]
        if not near:
            unexplained.append(j)
    if unexplained:
        findings.append(f'{len(unexplained)} jobs failed with no fault around them')

    for m in ('ctl',) + WORKERS:
        log = ssh(m, f'journalctl -u {unit(m)} --since "{since.replace("T", " ").rstrip("Z")} UTC" -o short-iso --no-pager',
                  timeout=60).stdout
        with open(os.path.join(out, f'{m}.log'), 'w') as f:
            f.write(log)
    noroom = sum('no room after all' in line for line in open(os.path.join(out, 'ctl.log')))
    with open(os.path.join(out, 'jobs.json'), 'w') as f:
        json.dump(js, f, indent=1)

    lines = [f'# Chaos run, seed {plan["seed"]}', '', f'Replay: `{replay} <out dir>`', '',
             f'{len(plan["events"])} steps over {plan["minutes"]:g} minutes; {len(js)} jobs, '
             f'{sum(x["state"] == "success" for x in js)} passed, {len(failed)} failed; '
             f'{noroom} placements the worker had no room for.', '',
             '## Verdict', '']
    lines += [f'- {x}' for x in findings] or ['- every check held']
    lines += [f'- note: {x}' for x in notes]
    lines += ['', '## What was done', '']
    with open(os.path.join(out, 'events.jsonl')) as f:
        lines += [f'- +{e["t"]}s {e["what"]}' for e in map(json.loads, f)]
    lines += ['', '## Failed jobs', '']
    lines += [f'- {j["forge"]} {j["name"]}: {j["state"]}, near {", ".join(j["faults"]) or "no fault"}' for j in failed] or ['- none']
    with open(os.path.join(out, 'report.md'), 'w') as f:
        f.write('\n'.join(lines) + '\n')
    print('\n'.join(lines))
    sys.exit(1 if findings else 0)


if __name__ == '__main__':
    main()
