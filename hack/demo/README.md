# The fleet demo

A video of a fleet from nothing: one controller and three workers in
"regions" (added network delay), `fleetci init`, the joins, the microVM
checks, then GitHub and GitLab jobs. The machines are cloud-hypervisor
microVMs in one container on a host with nested KVM.

    docker build -t fleetci-demo-lab hack/demo
    docker run -d --init --name fleetci-demo-lab --hostname lab \
      --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN \
      --sysctl net.ipv4.ip_forward=1 \
      -v $DATA:/data -v $IMAGES:/base:ro \
      -v $CLOUD_HYPERVISOR:/usr/local/bin/cloud-hypervisor:ro \
      -v $PWD/hack/demo:/demo:ro fleetci-demo-lab sleep infinity

`$DATA` holds `bin/` (fleetci, fleeting-plugin-fleetingd,
cloud-hypervisor) and `secrets/` (`github-app.pem`, `github-app.id`,
`runner.token`); `$IMAGES` holds fleetingd's `prebuilt-*` and `published-*`
images, from a worker that has run a job.

    docker exec fleetci-demo-lab /demo/reset.sh    # machines.sh, then provision.sh
    docker exec fleetci-demo-lab /demo/tools.sh
    python3 -I hack/demo/record.py $DATA/rec       # needs gh and glab logged in
    # speech: tts.py in python:3.12-slim with kokoro-onnx 0.4.9, into $DATA/rec/speech
    python3 -I hack/demo/render.py $DATA/rec demo.mp4   # numpy, pillow, ffmpeg
    docker exec fleetci-demo-lab /demo/rewind.sh   # back to before init, for another take

`record.py` types into a tmux session per pane and saves what each shows;
`render.py` lays them out, speeds up the waits (the header shows by how
much) and puts `narration.json` over it. The controller uses the same
GitLab runner token as any other fleetci serving it: stop that one first.
