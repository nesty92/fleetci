# VM images

Images to start from, built on GitHub's runners by
[images.yml](../.github/workflows/images.yml) and published as releases of
this repo. Each is for [fleetingd](https://github.com/nesty92/fleeting-plugin-fleetingd),
amd64 (arm64 once a GitHub runner for it has `/dev/kvm`): Ubuntu's daily
cloud image (the one fleetingd builds on) with what one CI system needs, and
not the other's tools.

Both have:

- Docker with buildx, the `ubuntu` user in the `docker` group;
- zstd, curl, cloud-init;
- snapd, unattended-upgrades and apt's timers removed or masked, so a VM's
  first minute is the job's;
- no gitlab-runner: fleetci's GitLab jobs run in containers, through
  docker-autoscaler.

## github

For GitHub Actions jobs, whose steps run on the VM itself, as on
`ubuntu-latest`. Defined in [github.json](github.json).

- actions/runner unpacked in `~/actions-runner-<version>`, with libicu and
  the rest of its dependencies, so no job downloads it;
- Docker compose;
- what workflows written for GitHub's runners most often call without
  installing it: git with git-lfs, the GitHub CLI (`gh`), `python` and pip,
  make and gcc (`build-essential`), zip and unzip.

## gitlab

For gitlab-runner's docker-autoscaler, through fleetci: each job and its
services run in containers, so the VM needs only Docker. Defined in
[gitlab.json](gitlab.json).

## Using one

`/etc/fleetci-image` says which image it is, the runner it holds and when it
was built. Each `.json` is a fleetingd `plugin_config` whose commands run
once in the image; a command that fails fails the build.

Each is rebuilt every Monday, and on other days when actions/runner has a new
release. Each build is a release `vm-image-<image>-<date>-<run>`, the four
newest kept; the release `vm-image-<image>` names the newest. Point a class
at it in place of `vm_prebuild_cloudinit_extra_cmds` (fleeting-plugin-fleetingd
with `vm_image_url`):

```toml
[classes.4vcpu-8gb.plugin_config]
vm_image_url = "https://github.com/nesty92/fleetci/releases/download/vm-image-github/github-{arch}.SHA256SUMS"
```

A class serving both GitHub and GitLab jobs takes `github`: it has all
`gitlab` has.

For everything `ubuntu-latest` has, build GitHub's own image from its
recipe: [runner-images](runner-images/README.md).

fleetingd reads the list on each start and downloads an image it doesn't
have yet; running VMs keep theirs. To stay on one build, use its own
release's URL. Leave `runner_version` unset and fleetci runs the runner the
image holds.

To build one yourself, as root on a host with `/dev/kvm`, cloud-hypervisor,
nft and qemu-img:

```sh
sed -e "s|@DIR@|/var/tmp/image|g" -e "s|@RUNNER_VERSION@|2.338.2|g" -e "s|@RUNNER_MIRROR@||g" images/github.json > github.json
fleeting-plugin-fleetingd build-image -config github.json -out out -name github
images/verify.sh out/github-amd64.qcow2 github 2.338.2
```

`@RUNNER_MIRROR@` empty downloads actions/runner from GitHub. A URL in its
place is where the build VM gets `actions-runner-linux-<arch>-<version>.tar.gz`
instead, such as an HTTP server on the build host; the workflow serves one
downloaded with its token.
