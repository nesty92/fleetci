#!/usr/bin/env python3
"""short: fleetci's logs, one short coloured line each, for the demo's panes.

    fleetci controller |& short

hclog's "2026-11-09T10:11:12.345Z [INFO]  fleetci.worker: placed: job=7 vm=a"
becomes "10:11:12 worker  placed  job=7 vm=a", without the keys nobody
watching needs, cut to the terminal's width. Other lines pass through.
"""
import os
import re
import shutil
import sys

HCLOG = re.compile(r'^\d{4}-\d\d-\d\dT(\d\d:\d\d:\d\d)\S*\s+\[(\w+)\]\s+(.*)$')
LOGRUS = re.compile(r'^time="\d{4}-\d\d-\d\dT(\d\d:\d\d:\d\d)\S*" level=(\w+) msg="((?:[^"\\]|\\.)*)"\s*(.*)$')
NAME = re.compile(r'^([\w.\-]+): (.*)$')
KV = re.compile(r'([@\w.\-]+)=("(?:[^"\\]|\\.)*"|\S+)')
# The keys only a debugger wants.
NOISE = {'timestamp', '@module', '@level', '@timestamp', 'plugin_version', 'plugin', 'path', 'pid',
         'metadata', 'version', 'subsystem'}
# Lines only a debugger wants: taskscaler's bookkeeping, the runners' chatter.
DROP = [re.compile(p) for p in [
    r'\.taskscaler: ', r'instance process finished', r'stopping instance', r'snapshot memory',
    r'waiting for the image directory', r'First VM requested', r'Appending trace', r'Submitting job',
    r'Updating job', r'Dialing instance', r'Removed \.(credentials|runner)', r'listener exit', r'runner version',
    r'Exiting runner', r'Listening for Jobs', r'Connected to GitHub', r'Initializing executor providers',
] + [p for p in os.environ.get('SHORT_DROP', '').split('|') if p]]

GREEN, YELLOW, RED, DIM, CYAN, BOLD, RESET = ('\033[32m', '\033[33m', '\033[31m', '\033[2m', '\033[36m',
                                              '\033[1m', '\033[0m')
GOOD = re.compile(r'\b(Ready|ready|placed|acquired|joined|passed|succeeded|success|ok|leader|started|done)\b')
BAD = re.compile(r'\b(FAIL\w*|fail\w*|error|refused|died)\b')


def width():
    return int(os.environ.get('COLUMNS') or shutil.get_terminal_size((100, 20)).columns)


def shorten(line):
    m = HCLOG.match(line)
    if not m:
        g = LOGRUS.match(line)
        if g:
            clock, level, msg, rest = g.groups()
            return level.upper().replace('WARNING', 'WARN'), f'{clock} runner {msg} {rest}'.strip()
        return None, line
    clock, level, rest = m.groups()
    name = ''
    n = NAME.match(rest)
    if n and '=' not in n.group(1):
        name, rest = n.groups()
        name = name.removeprefix('fleetci.').removeprefix('fleetci')
        # fleet.class.<class>.plugin.fleeting-plugin-fleetingd: <class> fleetingd
        name = re.sub(r'^fleet\.class\.([\w\-]+)(?:\.(?:plugin\.fleeting-plugin-)?|$)', r'\1 ', name)
    msg, kvs = rest, ''
    k = re.search(r'(?:^|: )([@\w.\-]+=)', rest)
    if k:
        msg, kvs = rest[:k.start()], rest[k.start(1):]
    # A plugin's line, relayed: "fleetingd: msg: k=v" again inside the message.
    msg = msg.removesuffix(':').strip()
    msg = re.sub(r'(^|: )\d{4}-\d\d-\d\d \d\d:\d\d:\d\dZ: ', r'\1', msg)  # the Actions runner's own clock
    pairs = [(a, b) for a, b in KV.findall(kvs) if a not in NOISE]
    text = ' '.join([clock, name, msg] + [f'{a}={b}' for a, b in pairs])
    return level, re.sub(r'\s+', ' ', text)


def colour(level, text, cols):
    text = text[:cols]
    if level is None:
        if text.startswith('ok'):
            return GREEN + text[:2] + RESET + text[2:]
        return text
    clock, _, body = text.partition(' ')
    c = {'WARN': YELLOW, 'ERROR': RED}.get(level, '')
    if c:
        body = c + body + RESET
    else:
        body = BAD.sub(lambda m: RED + m.group(0) + RESET, GOOD.sub(lambda m: GREEN + m.group(0) + RESET, body))
        body = re.sub(r'(\S+=)', DIM + r'\1' + RESET, body)
    return DIM + clock + RESET + ' ' + body


def main():
    for raw in sys.stdin:
        line = raw.rstrip('\n')
        if any(d.search(line) for d in DROP):
            continue
        level, text = shorten(line)
        if level == 'DEBUG' or level == 'TRACE':
            continue
        print(colour(level, text, width()), flush=True)


if __name__ == '__main__':
    try:
        main()
    except (KeyboardInterrupt, BrokenPipeError):
        pass
