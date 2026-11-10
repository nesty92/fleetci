#!/bin/sh
# prepare.sh <runner-images checkout> [build]: copies GitHub's Ubuntu build
# (default ubuntu-24_04) into images/ubuntu/fleetci of the checkout, beside
# fleetci.pkr.hcl, and changes only what fleetci needs:
#
# - the AMI and fleetingd sources in place of Azure's;
# - the SSH user in the docker group, and actions/runner unpacked in its
#   home, so no job downloads it;
# - cloud-init's own cleanup in place of the Azure agent's, the build's
#   password locked, and, for fleetingd, the kernel copied out.
#
# It stops when GitHub's build no longer looks as expected, rather than
# build something else.
set -eu
repo=${1:?usage: prepare.sh <runner-images checkout> [build]}
build=${2:-ubuntu-24_04}
runner=${RUNNER_VERSION:-2.338.2}
here=$(cd "$(dirname "$0")" && pwd)
src=$repo/images/ubuntu/templates
out=$repo/images/ubuntu/fleetci

[ -f "$src/build.$build.pkr.hcl" ] || { echo "no $src/build.$build.pkr.hcl" >&2; exit 1; }
mkdir -p "$out"
cp "$src/variable.ubuntu.pkr.hcl" "$here/fleetci.pkr.hcl" "$out/"

awk -v runner="$runner" '
  /sources = \["source.azure-arm.image"\]/ {
    sub(/source.azure-arm.image/, "source.amazon-ebs.image\", \"source.qemu.image"); sources++
  }
  /waagent -force -deprovision\+user/ {
    print "    inline          = ["
    print "      \"sleep 30\","
    print "      \"usermod -aG docker ubuntu\","
    print "      \"a=x64; [ $(uname -m) = aarch64 ] && a=arm64; d=/home/ubuntu/actions-runner-" runner "; mkdir -p $d && curl -fsSL https://github.com/actions/runner/releases/download/v" runner "/actions-runner-linux-$a-" runner ".tar.gz | tar -xz -C $d && chown -R ubuntu:ubuntu $d\","
    print "      \"install -m 644 $(readlink -f /boot/vmlinuz) /tmp/vmlinuz\","
    print "      \"passwd -l ubuntu\","
    print "      \"cloud-init clean --logs --machine-id\","
    print "      \"export HISTSIZE=0 && sync\","
    print "    ]"
    print "  }"
    print ""
    print "  provisioner \"file\" {"
    print "    only        = [\"qemu.image\"]"
    print "    direction   = \"download\""
    print "    source      = \"/tmp/vmlinuz\""
    print "    destination = \"output-fleetingd/runner-images-${var.fleetci_arch}.vmlinuz\""
    cleanup++
    next
  }
  { print }
  END { if (sources != 1 || cleanup != 1) { print "GitHub'\''s build changed: sources " sources ", cleanup " cleanup > "/dev/stderr"; exit 1 } }
' "$src/build.$build.pkr.hcl" > "$out/build.$build.pkr.hcl"

echo "ready: cd $out && packer init . && packer build -only '$build.amazon-ebs.image' ."
