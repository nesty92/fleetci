# VM images

Images to start from, built on GitHub's runners by
[images.yml](../.github/workflows/images.yml) and published as releases of
this repo.

## small

For [fleetingd](https://github.com/nesty92/fleeting-plugin-fleetingd), amd64
(arm64 once a GitHub runner for it has `/dev/kvm`): Ubuntu's daily cloud image (the one fleetingd builds on) with
what fleetci needs and little else.

- Docker with buildx and compose, the `ubuntu` user in the `docker` group;
- actions/runner unpacked in `~/actions-runner-<version>`, with libicu and
  the rest of its dependencies;
- what workflows written for GitHub's runners most often call without
  installing it: git with git-lfs, the GitHub CLI (`gh`), `python` and pip,
  make and gcc (`build-essential`), zip and unzip;
- zstd, curl, cloud-init;
- snapd, unattended-upgrades and apt's timers removed or masked, so a VM's
  first minute is the job's.

`/etc/fleetci-image` says which runner it holds and when it was built.
Defined in [small.json](small.json), a fleetingd `plugin_config` whose
commands run once in the image.

It is rebuilt every Monday, and on other days when actions/runner has a new
release. Each build is a release `vm-image-small-<date>-<run>`, the four
newest kept; the release `vm-image-small` names the newest. Point a class at
it in place of `vm_prebuild_cloudinit_extra_cmds` (fleeting-plugin-fleetingd
with `vm_image_url`):

```toml
[classes.4vcpu-8gb.plugin_config]
vm_image_url = "https://github.com/nesty92/fleetci/releases/download/vm-image-small/small-{arch}.SHA256SUMS"
```

fleetingd reads the list on each start and downloads an image it doesn't
have yet; running VMs keep theirs. To stay on one build, use its own
release's URL. Leave `runner_version` unset and fleetci runs the runner the
image holds.

To build it yourself, as root on a host with `/dev/kvm`, cloud-hypervisor,
nft and qemu-img:

```sh
sed -e "s|@DIR@|/var/tmp/image|g" -e "s|@RUNNER_VERSION@|2.338.2|g" images/small.json > small.json
fleeting-plugin-fleetingd build-image -config small.json -out out -name small
images/verify.sh out/small-amd64.qcow2 2.338.2
```
