# Speaks each beat of narration.json with Kokoro (af_heart), one wav each,
# and their lengths in speech.json. Runs in python:3.12-slim with
# kokoro-onnx 0.4.9; see render.sh. The voice and the trimming follow
# the browsing project's videos.
import json, re, sys
import numpy as np, soundfile as sf
from kokoro_onnx import Kokoro

SAY = [("fleetci", "fleet C I"), ("microVMs", "micro V Ms"), ("microVM", "micro V M"), ("CPUs", "C P Us"),
       ("CPU", "C P U"), ("CI", "C I"), ("VMs", "V Ms"), ("VM", "V M"), ("GitLab", "Git Lab")]


def say(t):
    for a, b in SAY:
        t = re.sub(rf"(?<![\w-]){re.escape(a)}(?![\w-])", b, t)
    return t.replace("…", "...")


model, out = sys.argv[1], sys.argv[2]
k = Kokoro(f"{model}/kokoro-v1.0.onnx", f"{model}/voices-v1.0.bin")
lengths = {}
for b in json.load(open("narration.json")):
    if not b.get("text"):
        continue
    a, sr = k.create(say(b["text"]), voice="af_heart", speed=1.0, lang="en-us")
    nz = np.nonzero(np.abs(a) > 0.01)[0]
    a = a[max(0, nz[0] - 600): nz[-1] + 2400]
    sf.write(f"{out}/{b['beat']}.wav", a, sr)
    lengths[b["beat"]] = round(len(a) / sr, 3)
json.dump(lengths, open(f"{out}/speech.json", "w"), indent=1)
print(lengths)
