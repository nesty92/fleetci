# The fleet demo

A video of a fleet from nothing, on the lab in `hack/lab`: `fleetci init`
on the controller, a token per worker, the one-line join on three bare
machines, the microVM checks, then GitHub and GitLab jobs. Midway a GitLab
job's microVM is killed: the worker fails that job at once and boots a
replacement, and the retried job passes.

    docker exec fleetci-lab /lab/rewind.sh        # or reset.sh, the first time
    cp -r hack/demo $DATA/demo && docker exec fleetci-lab sh /data/demo/tools.sh
    python3 -I hack/demo/record.py $DATA/rec      # needs gh and glab logged in
    # speech: tts.py in python:3.12-slim with kokoro-onnx 0.4.9, into $DATA/rec/speech
    python3 -I hack/demo/render.py $DATA/rec demo.mp4   # numpy, pillow, ffmpeg

`record.py` types into a tmux session per pane, saves what each shows and
marks the beats; `render.py` lays the panes out, speeds up the waits (the
header shows by how much), zooms on the pane that matters and puts
`narration.json` over it. `STILLS=4,66,…` renders those seconds as PNGs
instead of the video. The controller uses the same GitLab runner token as
any other fleetci serving it: stop that one first.
