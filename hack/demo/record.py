#!/usr/bin/env python3
"""record.py: drives the demo and records it, for render.py.

    python3 -I record.py <out dir>

The lab (fleetci-demo-lab, after machines.sh, provision.sh and tools.sh)
is four machines that have nothing running yet. This types the demo into
a terminal per pane, each a tmux session on a private server, and saves
what every terminal shows four times a second (<out>/screens.jsonl), with
the moments the narration starts at (<out>/beats.jsonl).
"""
import json
import os
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import layout  # noqa: E402

LAB = 'fleetci-demo-lab'
TMUX = ['tmux', '-L', 'fleetci-demo']
HERE = os.path.dirname(os.path.abspath(__file__))
GH_REPO = 'WireZone/fleetci-playground'
GL_REPO = 'Nesty92/fleetci-playground'


class Recorder:
    def __init__(self, out):
        self.out, self.t0 = out, time.time()
        self.screens = open(os.path.join(out, 'screens.jsonl'), 'w')
        self.beats = open(os.path.join(out, 'beats.jsonl'), 'w')
        self.last, self.stop = {}, threading.Event()

    def now(self):
        return round(time.time() - self.t0, 3)

    def beat(self, name):
        print(f'{self.now():7.1f}  {name}', flush=True)
        self.beats.write(json.dumps({'t': self.now(), 'beat': name}) + '\n')
        self.beats.flush()

    def sample(self):
        while not self.stop.is_set():
            t = self.now()
            for p in layout.PANES:
                r = subprocess.run(TMUX + ['capture-pane', '-p', '-e', '-t', p], capture_output=True, text=True)
                c = subprocess.run(TMUX + ['display', '-p', '-t', p, '#{cursor_x} #{cursor_y}'],
                                   capture_output=True, text=True).stdout.split()
                screen = (r.stdout.split('\n'), [int(x) for x in c] if len(c) == 2 else None)
                if self.last.get(p) != screen:
                    self.last[p] = screen
                    self.screens.write(json.dumps({'t': t, 'pane': p, 'lines': screen[0], 'cursor': screen[1]}) + '\n')
            self.screens.flush()
            time.sleep(max(0, 0.25 - (self.now() - t)))


def tmux(*args):
    subprocess.run(TMUX + list(args), check=True)


def keys(pane, text, enter=True, cps=30):
    """Types text into pane, a key at a time, as a person would."""
    for ch in text:
        # A lone ; is tmux's command separator.
        tmux('send-keys', '-t', pane, '-l', '\\;' if ch == ';' else ch)
        time.sleep(1 / cps)
    if enter:
        time.sleep(0.25)
        tmux('send-keys', '-t', pane, 'Enter')


def together(*fns):
    ts = [threading.Thread(target=f) for f in fns]
    for t in ts:
        t.start()
    for t in ts:
        t.join()


def lab(machine, cmd):
    return subprocess.run(['docker', 'exec', LAB, '/data/ssh.sh', machine, cmd], capture_output=True, text=True)


def status():
    r = lab('ctl', 'fleetci fleet status -json')
    try:
        return json.loads(r.stdout) if r.returncode == 0 else None
    except json.JSONDecodeError:
        return None


def until(what, cond, timeout, every=1.0):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(every)
    raise SystemExit(f'gave up waiting: {what}')


def forge_state(out):
    try:
        with open(os.path.join(out, 'forge.json')) as f:
            return json.load(f)
    except (OSError, json.JSONDecodeError):
        return None


def main():
    out = os.path.abspath(sys.argv[1])
    os.makedirs(out, exist_ok=True)
    subprocess.run(TMUX + ['kill-server'], capture_output=True)
    since = datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    for p in layout.PANES:
        cols, rows = layout.size(p)
        if p == 'forge':
            cmd = f'python3 -I {HERE}/forge.py {since} {out}/forge.json'
        else:
            cmd = f'docker exec -it {LAB} /data/ssh.sh {"ctl" if p == "admin" else p}'
        tmux('new-session', '-d', '-s', p, '-x', str(cols), '-y', str(rows), cmd)
        tmux('set', '-t', p, 'status', 'off')
    time.sleep(4)
    for p in layout.PANES:
        if p != 'forge':
            tmux('send-keys', '-t', p, 'clear', 'Enter')
    time.sleep(1.5)

    r = Recorder(out)
    threading.Thread(target=r.sample, daemon=True).start()
    time.sleep(1)

    r.beat('lab')
    time.sleep(1)
    together(*[lambda m=m: keys(m, 'ping -c 3 ctl') for m in ('eu', 'us', 'ap')])
    time.sleep(4)

    r.beat('init')
    keys('ctl', 'fleetci init')
    time.sleep(3)

    r.beat('controller')
    keys('ctl', 'fleetci controller |& short')
    until('a leader', lambda: (status() or {}).get('leader'), 60)
    time.sleep(2)

    r.beat('token')
    keys('admin', 'fleetci token create -ttl 15m')
    time.sleep(3)
    keys('admin', "for m in eu us ap; do fleetci token create -ttl 15m | head -1 | ssh $m 'cat > token'; done")
    time.sleep(3)
    keys('admin', 'fleetwatch')
    time.sleep(2)

    r.beat('join')
    join = 'fleetci join -token $(cat token) -labels region={m} -cpus 2 -memory 4GB -foreground ctl |& short'
    together(*[lambda m=m, d=d: (time.sleep(d), keys(m, join.format(m=m))) for m, d in (('eu', 0), ('us', 0.6), ('ap', 1.2))])
    until('three workers', lambda: len((status() or {}).get('workers') or []) == 3, 120)

    r.beat('check')
    until('three ready', lambda: sum(w['state'] == 'Ready' for w in (status() or {}).get('workers') or []) == 3,
          600, every=0.5)

    r.beat('ready')
    time.sleep(5)

    r.beat('jobs')
    together(
        lambda: subprocess.run(['gh', 'workflow', 'run', 'burst.yml', '-R', GH_REPO], check=True),
        lambda: subprocess.run(['gh', 'workflow', 'run', 'basic.yml', '-R', GH_REPO], check=True),
        lambda: subprocess.run(['glab', 'ci', 'run', '-R', GL_REPO, '-b', 'demo'], check=True,
                               capture_output=True),
    )
    until('the jobs running', lambda: any((w.get('report') or {}).get('vms') for w in (status() or {}).get('workers') or []),
          300)

    r.beat('running')
    total = lambda s: sum(f['jobs'] for f in s.values())  # noqa: E731
    until('the jobs done', lambda: (s := forge_state(out)) and total(s) >= 9
          and sum(f['done'] for f in s.values()) == total(s), 900, every=2)

    r.beat('done')
    time.sleep(8)
    r.beat('end')
    r.stop.set()
    time.sleep(0.5)
    print(json.dumps(forge_state(out)))


if __name__ == '__main__':
    main()
