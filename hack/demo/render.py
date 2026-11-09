#!/usr/bin/env python3
"""render.py: the recording as a video, with the narration.

    python3 -I render.py <recording dir> <out.mp4>

Reads what record.py saved (screens.jsonl, beats.jsonl, forge.json) and
what tts.py spoke (<dir>/speech/<beat>.wav, speech.json). Each beat of
narration.json lasts as long as its speech, or longer: a beat whose real
time is longer plays faster, with its speed shown; a shorter one holds
its last frame. Frames go straight to ffmpeg.
"""
import bisect
import json
import os
import re
import subprocess
import sys
import wave

import numpy as np
from PIL import Image, ImageDraw, ImageFont

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import layout as L  # noqa: E402

FPS = 30
MAX_SPEED = 12
MIN_BEAT = {'end': 2.0}
LEAD = 0.35  # speech starts this long into its beat
AFTER = 0.9  # and the beat lasts this long after it

MONO = '/usr/share/fonts/TTF/DejaVuSansMono.ttf'
MONO_B = '/usr/share/fonts/TTF/DejaVuSansMono-Bold.ttf'
SANS = '/usr/share/fonts/noto/NotoSans-Regular.ttf'
SANS_M = '/usr/share/fonts/noto/NotoSans-Medium.ttf'
SANS_B = '/usr/share/fonts/noto/NotoSans-Bold.ttf'


def hexc(s):
    return tuple(int(s[i:i + 2], 16) for i in (1, 3, 5))


INK = hexc('#090d12')
PANE = hexc('#0e141b')
TITLEBAR = hexc('#131b24')
EDGE = hexc('#1f2a36')
FG = hexc('#c8d3de')
MUTED = hexc('#6b7a8a')
ACCENT = hexc('#3dd6c6')
REGION = {'ctl': ACCENT, 'admin': ACCENT, 'eu': hexc('#7aa7ff'), 'us': hexc('#f2c46b'), 'ap': hexc('#d79bff'),
          'forge': hexc('#c8d3de')}
ANSI = [hexc(c) for c in ('#1b2530', '#ff7b72', '#7ee2a8', '#f2c46b', '#7aa7ff', '#d79bff', '#66d9e8', '#c8d3de',
                          '#4b5866', '#ffa198', '#a6f0c4', '#ffdc8f', '#a5c4ff', '#e6bfff', '#9be9f2', '#ffffff')]

mono = ImageFont.truetype(MONO, L.FONT_PX)
mono_b = ImageFont.truetype(MONO_B, L.FONT_PX)


def xterm256(n):
    if n < 16:
        return ANSI[n]
    if n < 232:
        n -= 16
        steps = (0, 95, 135, 175, 215, 255)
        return steps[n // 36], steps[n // 6 % 6], steps[n % 6]
    v = 8 + (n - 232) * 10
    return v, v, v


SGR = re.compile(r'\x1b\[([\d;]*)m')


def runs(line):
    """(text, fg, bg, bold, dim) runs of one captured line."""
    fg, bg, bold, dim, rev = None, None, False, False, False
    out, pos = [], 0
    for m in list(SGR.finditer(line)) + [None]:
        text = line[pos:m.start() if m else len(line)]
        if text:
            f, b = (bg, fg) if rev else (fg, bg)
            out.append((text, f, b, bold, dim))
        if not m:
            break
        pos = m.end()
        codes = [int(c) if c else 0 for c in m.group(1).split(';')] or [0]
        i = 0
        while i < len(codes):
            c = codes[i]
            if c == 0:
                fg, bg, bold, dim, rev = None, None, False, False, False
            elif c == 1:
                bold = True
            elif c == 2:
                dim = True
            elif c == 22:
                bold = dim = False
            elif c == 7:
                rev = True
            elif c == 27:
                rev = False
            elif 30 <= c <= 37:
                fg = ANSI[c - 30]
            elif 90 <= c <= 97:
                fg = ANSI[c - 82]
            elif c == 39:
                fg = None
            elif 40 <= c <= 47:
                bg = ANSI[c - 40]
            elif 100 <= c <= 107:
                bg = ANSI[c - 92]
            elif c == 49:
                bg = None
            elif c in (38, 48) and i + 1 < len(codes):
                if codes[i + 1] == 5 and i + 2 < len(codes):
                    col = xterm256(codes[i + 2])
                    i += 2
                elif codes[i + 1] == 2 and i + 4 < len(codes):
                    col = tuple(codes[i + 2:i + 5])
                    i += 4
                else:
                    col = None
                if c == 38:
                    fg = col
                else:
                    bg = col
            i += 1
    return out


def mix(a, b, t):
    return tuple(int(a[i] * (1 - t) + b[i] * t) for i in range(3))


# What short.py did not shorten yet when the demo was recorded.
TIDY = [(re.compile(r'(runner: )\d{4}-\d\d-\d\d \d\d:\d\d:\d\dZ: '), r'\1'), (re.compile(r'fleet\.class\.([\w\-]+) '), r'\1 ')]


def pane_image(name, lines, cursor, focused):
    x, y, w, h, title = L.PANES[name]
    img = Image.new('RGB', (w, h), PANE)
    d = ImageDraw.Draw(img)
    colour = REGION[name]
    d.rectangle([0, 0, w, L.TITLE], fill=TITLEBAR)
    d.ellipse([12, L.TITLE // 2 - 4, 20, L.TITLE // 2 + 4], fill=colour)
    d.text((28, L.TITLE // 2), title, font=ImageFont.truetype(SANS_M, 14), fill=FG if focused else MUTED, anchor='lm')
    cols, rows = L.size(name)
    for r, line in enumerate(lines[:rows]):
        cx = 0
        ty = L.TITLE + L.PAD + r * L.LINE_H
        for a, b in TIDY:
            line = a.sub(b, line)
        for text, fg, bg, bold, dim in runs(line):
            text = text[:max(0, cols - cx)]
            if not text:
                continue
            px = L.PAD + cx * L.CELL_W
            if bg:
                d.rectangle([px, ty, px + len(text) * L.CELL_W, ty + L.LINE_H - 1], fill=bg)
            f = fg or FG
            if dim:
                f = mix(f, PANE, 0.45)
            d.text((px, ty + 1), text, font=mono_b if bold else mono, fill=f)
            cx += len(text)
    if cursor and name not in ('forge',):
        cx, cy = cursor
        if cy < rows:
            px, ty = L.PAD + cx * L.CELL_W, L.TITLE + L.PAD + cy * L.LINE_H
            d.rectangle([px, ty + 1, px + L.CELL_W - 1, ty + L.LINE_H - 1], fill=mix(FG, PANE, 0.35))
    d.rectangle([0, 0, w - 1, h - 1], outline=colour if focused else EDGE, width=2 if focused else 1)
    if not focused:
        img = Image.blend(img, Image.new('RGB', (w, h), INK), 0.28)
    return img


class Screens:
    def __init__(self, path):
        self.t, self.s = {p: [] for p in L.PANES}, {p: [] for p in L.PANES}
        with open(path) as f:
            for line in f:
                e = json.loads(line)
                self.t[e['pane']].append(e['t'])
                self.s[e['pane']].append((e['lines'], e['cursor']))

    def at(self, pane, t):
        i = bisect.bisect_right(self.t[pane], t) - 1
        return i


CHAPTERS = ['Machines', 'Controller', 'Tokens', 'Join', 'Check', 'Ready', 'Jobs', 'Done']


def header(chapter, speed, real):
    img = Image.new('RGB', (L.W, L.HEADER), INK)
    d = ImageDraw.Draw(img)
    d.text((L.GAP + 2, L.HEADER // 2), 'fleetci', font=ImageFont.truetype(SANS_B, 28), fill=ACCENT, anchor='lm')
    d.text((132, L.HEADER // 2 + 1), '1 controller · 3 regions · GitHub + GitLab', font=ImageFont.truetype(SANS, 17),
           fill=MUTED, anchor='lm')
    f = ImageFont.truetype(SANS_M, 16)
    cur = CHAPTERS.index(chapter) if chapter in CHAPTERS else -1
    x = 600
    for i, c in enumerate(CHAPTERS):
        label = f'{i + 1}  {c}'
        tw = d.textlength(label, font=f)
        if i == cur:
            d.rounded_rectangle([x - 12, 16, x + tw + 12, L.HEADER - 16], radius=16, fill=hexc('#12403d'))
            d.text((x, L.HEADER // 2), label, font=f, fill=ACCENT, anchor='lm')
        else:
            d.text((x, L.HEADER // 2), label, font=f, fill=FG if i < cur else hexc('#3a4654'), anchor='lm')
        x += tw + 38
    right = L.W - L.GAP - 4
    m, s = divmod(int(real), 60)
    d.text((right, L.HEADER // 2), f'real time {m:02d}:{s:02d}', font=ImageFont.truetype(MONO, 16), fill=MUTED,
           anchor='rm')
    if speed > 1.25:
        label = f'×{speed:.0f}' if speed >= 3 else f'×{speed:.1f}'
        bf = ImageFont.truetype(SANS_B, 16)
        tw = d.textlength(label, font=bf)
        bx = right - 168
        d.rounded_rectangle([bx - tw - 12, 18, bx, L.HEADER - 18], radius=12, fill=hexc('#3d3315'))
        d.text((bx - 6, L.HEADER // 2), label, font=bf, fill=REGION['us'], anchor='rm')
    return img


def wrap(text, font, width, d):
    words, lines, cur = text.split(), [], ''
    for w in words:
        t = (cur + ' ' + w).strip()
        if d.textlength(t, font=font) > width and cur:
            lines.append(cur)
            cur = w
        else:
            cur = t
    return lines + [cur] if cur else lines


def footer(text):
    img = Image.new('RGB', (L.W, L.FOOTER), INK)
    if text:
        d = ImageDraw.Draw(img)
        f = ImageFont.truetype(SANS, 23)
        lines = wrap(text, f, L.W - 240, d)[-2:] if len(wrap(text, f, L.W - 240, d)) <= 2 else None
        if lines is None:
            f = ImageFont.truetype(SANS, 20)
            lines = wrap(text, f, L.W - 160, d)
        n = len(lines)
        for i, line in enumerate(lines):
            d.text((L.W // 2, L.FOOTER // 2 + (i - (n - 1) / 2) * 30), line, font=f, fill=FG, anchor='mm')
    return img


def card(kind, text_lines):
    img = Image.new('RGB', (L.W, L.H), INK)
    d = ImageDraw.Draw(img)
    d.text((L.W // 2, 250), 'fleetci', font=ImageFont.truetype(SANS_B, 110), fill=ACCENT, anchor='mm')
    for i, (t, size, col) in enumerate(text_lines):
        d.text((L.W // 2, 360 + i * 46), t, font=ImageFont.truetype(SANS, size), fill=col, anchor='mm')
    # The lab: the controller in the middle, the regions around it.
    cx, cy = L.W // 2, 600
    spots = {'eu': (cx - 420, cy + 110, '+20 ms'), 'us': (cx, cy + 190, '+80 ms'), 'ap': (cx + 420, cy + 110, '+160 ms')}
    lf = ImageFont.truetype(SANS_M, 22)
    sf = ImageFont.truetype(MONO, 17)
    for name, (x, y, ms) in spots.items():
        d.line([cx, cy, x, y], fill=EDGE, width=3)
        mx, my = (cx + x) // 2, (cy + y) // 2
        d.text((mx + (0 if name != 'us' else 14), my - 16 if name != 'us' else my), ms, font=sf, fill=MUTED,
               anchor='mm' if name != 'us' else 'lm')
    for name, (x, y, _) in [('ctl', (cx, cy, ''))] + list(spots.items()):
        c = REGION[name]
        d.rounded_rectangle([x - 70, y - 26, x + 70, y + 26], radius=12, fill=PANE, outline=c, width=2)
        d.text((x, y), {'ctl': 'controller'}.get(name, f'worker {name}'), font=lf, fill=c, anchor='mm')
    return img


def wav(path):
    with wave.open(path) as w:
        a = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16 if w.getsampwidth() == 2 else np.int32)
        return a.astype(np.float32) / (32768 if w.getsampwidth() == 2 else 2 ** 31), w.getframerate()


def main():
    rec, out = sys.argv[1], sys.argv[2]
    here = os.path.dirname(os.path.abspath(__file__))
    script = {b['beat']: b for b in json.load(open(os.path.join(rec, 'narration.json')
                                                      if os.path.exists(os.path.join(rec, 'narration.json'))
                                                      else os.path.join(here, 'narration.json')))}
    order = [b for b in script if b in ('intro', 'outro') or b in {x['beat'] for x in map(json.loads, open(os.path.join(rec, 'beats.jsonl')))}]
    speech = json.load(open(os.path.join(rec, 'speech', 'speech.json')))
    beats = [json.loads(line) for line in open(os.path.join(rec, 'beats.jsonl'))]
    real = {b['beat']: b['t'] for b in beats}
    screens = Screens(os.path.join(rec, 'screens.jsonl'))
    t_end = beats[-1]['t'] + 2.0

    # The timeline: (beat, video start, video length, real start, real length).
    timeline, v = [], 0.0
    for name in order:
        sp = speech.get(name, 0)
        if name in ('intro', 'outro'):
            dur = sp + LEAD + 1.4
            timeline.append((name, v, dur, None, 0))
        else:
            r0 = real[name]
            nxt = [b['t'] for b in beats if b['t'] > r0]
            r1 = nxt[0] if nxt else t_end
            dur = max(sp + LEAD + AFTER if sp else 0, MIN_BEAT.get(name, 2.5), (r1 - r0) / MAX_SPEED)
            timeline.append((name, v, dur, r0, r1 - r0))
        v += dur
    total = v
    print(f'video {total:.1f}s, real {t_end:.1f}s', flush=True)
    for name, v0, dur, r0, rl in timeline:
        print(f'  {name:<11} {v0:6.1f} +{dur:5.1f}  real {rl:6.1f}  x{(rl / dur if rl else 0):.1f}')

    # The narration on that timeline.
    sr = 24000
    track = np.zeros(int((total + 1) * sr), dtype=np.float32)
    for name, v0, dur, _, _ in timeline:
        p = os.path.join(rec, 'speech', f'{name}.wav')
        if os.path.exists(p):
            a, sr = wav(p)
            i = int((v0 + LEAD) * sr)
            track[i:i + len(a)] += a[:len(track) - i]
    with wave.open(os.path.join(rec, 'narration.wav'), 'w') as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes((np.clip(track, -1, 1) * 32767).astype(np.int16).tobytes())

    intro = card('intro', [('one controller · three regions · GitHub and GitLab jobs', 34, FG),
                           ('a fresh microVM for every job', 26, MUTED)])
    state = json.load(open(os.path.join(rec, 'forge.json')))
    n = sum(f['jobs'] for f in state.values())
    ok = sum(f['ok'] for f in state.values())
    outro = card('outro', [(f'{n} jobs · {ok} passed · {n} microVMs · 3 regions', 34, FG),
                           ('github.com/nesty92/fleetci', 26, MUTED)])

    stills = [float(x) for x in os.environ.get('STILLS', '').split(',') if x]
    if stills:
        class Stills:
            def __init__(self):
                self.k = 0

            def write(self, data):
                vt = self.k / FPS
                self.k += 1
                for t in stills:
                    if abs(vt - t) < 0.5 / FPS:
                        Image.frombytes('RGB', (L.W, L.H), data).save(f'{out}-{t:05.1f}.png')

            def close(self):
                pass

        class FF:
            stdin = Stills()

            def wait(self):
                pass
        ff = FF()
        nframes_max = int(max(stills) * FPS) + 2
    ff = ff if stills else subprocess.Popen(['ffmpeg', '-y', '-loglevel', 'error', '-f', 'rawvideo', '-pix_fmt', 'rgb24', '-s',
                           f'{L.W}x{L.H}', '-r', str(FPS), '-i', '-', '-i', os.path.join(rec, 'narration.wav'),
                           '-map', '0:v', '-map', '1:a', '-c:v', 'libx264', '-preset', 'slow', '-crf', '18',
                           '-pix_fmt', 'yuv420p', '-af',
                           'loudnorm=I=-16:TP=-1.5:LRA=11,aformat=sample_rates=48000:channel_layouts=stereo',
                           '-c:a', 'aac', '-b:a', '192k', '-shortest', '-movflags', '+faststart', out],
                          stdin=subprocess.PIPE)
    cache, last_grid = {}, None
    nframes = int(total * FPS) + 1
    if stills:
        nframes = min(nframes, nframes_max)
    for k in range(nframes):
        vt = k / FPS
        seg = next((s for s in timeline if s[1] <= vt < s[1] + s[2]), timeline[-1])
        name, v0, dur, r0, rl = seg
        if name in ('intro', 'outro'):
            img = intro if name == 'intro' else outro
            # Fade in from black at the start, and to the grid and back.
            u = vt - v0
            fade = min(1.0, u / 0.6, (dur - u) / 0.6) if name == 'intro' else min(1.0, u / 0.6, (dur - u) / 1.0)
            if name == 'intro' and u > dur - 0.6 and last_grid is None:
                fade = 1.0
            img = img.copy()
            img.paste(footer(script[name]['text']), (0, L.H - L.FOOTER - 40))
            frame = Image.blend(Image.new('RGB', (L.W, L.H), INK), img, max(0.0, fade))
        else:
            speed = rl / dur if rl > dur else 1.0
            rt = r0 + min(vt - v0, rl) if speed == 1.0 else r0 + (vt - v0) * speed
            b = script[name]
            frame = Image.new('RGB', (L.W, L.H), INK)
            for p in L.PANES:
                i = screens.at(p, rt)
                focused = p in b['focus']
                key = (i, focused)
                if cache.get(p, (None,))[0] != key:
                    lines, cursor = screens.s[p][i] if i >= 0 else ([], None)
                    cache[p] = (key, pane_image(p, lines, cursor, focused))
                x, y, _, _, _ = L.PANES[p]
                frame.paste(cache[p][1], (x, y))
            frame.paste(header(b['chapter'], speed, rt), (0, 0))
            frame.paste(footer(b['text'] if vt - v0 < (speech.get(name, 0) + LEAD + AFTER + 0.5) else ''),
                        (0, L.H - L.FOOTER))
            last_grid = frame
        ff.stdin.write(frame.tobytes())
        if k % (FPS * 10) == 0:
            print(f'  frame {k}/{nframes}', flush=True)
    ff.stdin.close()
    ff.wait()
    print(out)


if __name__ == '__main__':
    main()
