# GitHub's runner image

fleetci's [own images](../README.md) hold what most workflows call. For
workflows that need everything `ubuntu-latest` has, every toolchain,
browser and CLI, build GitHub's image yourself, from the recipe GitHub
builds it with: [actions/runner-images](https://github.com/actions/runner-images).
fleetci doesn't publish it: it is 75 GB, takes a few hours to build, and
GitHub releases a new one every week, so build the release you want, when
you want it.

GitHub's templates build only for Azure. [prepare.sh](prepare.sh) copies
GitHub's Ubuntu build, unchanged but for what fleetci needs:

- an AMI or a fleetingd image ([fleetci.pkr.hcl](fleetci.pkr.hcl)) in place
  of the Azure image;
- the `ubuntu` user, which fleetci connects as, in the `docker` group;
- actions/runner unpacked in `~/actions-runner-<version>` (`RUNNER_VERSION`,
  default 2.338.2), so no job downloads it;
- cloud-init's cleanup in place of the Azure agent's, and, for fleetingd,
  the image's kernel copied out beside it.

GitHub's install scripts, its tool versions and its own tests at the end of
the build are theirs. prepare.sh stops when GitHub's build no longer looks
as it expects, rather than build something else.

## Build it

With [Packer](https://developer.hashicorp.com/packer/install), from a
runner-images release (`ubuntu24/<date>` on
[its releases](https://github.com/actions/runner-images/releases)):

```sh
git clone --depth 1 -b ubuntu24/20261101.360 https://github.com/actions/runner-images
images/runner-images/prepare.sh runner-images            # ubuntu-24_04-arm64 for arm64
cd runner-images/images/ubuntu/fleetci
packer init .
```

### An AMI

With AWS credentials, on an instance Packer starts and removes:

```sh
packer build -only 'ubuntu-24_04.amazon-ebs.image' -var fleetci_aws_region=eu-north-1 .
```

For arm64, prepare `ubuntu-24_04-arm64` and build
`-only 'ubuntu-24_04-arm64.amazon-ebs.image' -var fleetci_arch=arm64`. The
AMI goes where the class's image goes: the launch template of its group, or
the pool's `ami_id` with the [terraform module](../../docs/runbooks/aws-with-the-module.md).

### A fleetingd image

On an amd64 host with `/dev/kvm`, QEMU and `xorriso` (for the build's
cloud-init seed), then compressed and listed as fleetingd's `vm_image_url`
expects:

```sh
packer build -only 'ubuntu-24_04.qemu.image' .
cd output-fleetingd
qemu-img convert -c -O qcow2 runner-images-amd64.qcow2 z.qcow2 && mv z.qcow2 runner-images-amd64.qcow2
sha256sum runner-images-amd64.qcow2 runner-images-amd64.vmlinuz > runner-images-amd64.SHA256SUMS
```

Put the three files where the workers can download them, such as a bucket
or a release, and point a class at the list. Its disk must be at least the
image's 75 GB; each VM's disk holds only what its job writes. In a fleet's
`controller.toml`:

```toml
[classes.4vcpu-16gb-full]
cpus = 4
memory = "16GB"
disk = "80GB"
image = "https://example.com/runner-images/runner-images-{arch}.SHA256SUMS"
```

or, in a class's `plugin_config`, `vm_image_url` with `vm_disk_size_gb = 80`.
Each worker downloads the image once, and again when the list names a new
one.

Leave the class's `runner_version` unset, and fleetci runs the runner the
image holds.
