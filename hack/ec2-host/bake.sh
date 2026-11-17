#!/bin/sh
# bake.sh: makes the baseline VMs' AMI from the recipe of the microVMs'
# image (images/github.json): Ubuntu 24.04, Docker, actions/runner and the
# same tools, so the two arms run the same software and differ only by
# where the VM runs.
#
#   AWS_PROFILE=… AWS_REGION=… ./bake.sh            prints the new AMI's id
#   AWS_PROFILE=… AWS_REGION=… ./bake.sh delete ami-…   deregisters it and deletes its snapshot
#
# It starts one c8i.large in the default VPC with the recipe as user-data,
# no SSH key and no role; the instance stops itself when done, its console
# says whether every command passed, and the AMI is made from it. About 15
# minutes; the instance is terminated either way.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
: "${AWS_REGION:?set AWS_REGION}"
export AWS_REGION AWS_PAGER=""

if [ "${1:-}" = delete ]; then
  ami=${2:?ami id}
  snaps=$(aws ec2 describe-images --image-ids "$ami" --query 'Images[0].BlockDeviceMappings[].Ebs.SnapshotId' --output text)
  aws ec2 deregister-image --image-id "$ami"
  for s in $snaps; do aws ec2 delete-snapshot --snapshot-id "$s"; done
  echo "deleted $ami and $snaps"
  exit 0
fi

runner=${RUNNER_VERSION:-2.338.2} # fleetci's DefaultRunnerVersion, as the published image
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The recipe's commands, in order; any failure leaves the marker out.
python3 -I - "$here/../../images/github.json" "$runner" > "$work/user-data" <<'PY'
import json, shlex, sys
cmds = json.load(open(sys.argv[1]))["vm_prebuild_cloudinit_extra_cmds"]
print("#!/bin/bash")
print("exec > >(tee /dev/console) 2>&1")
print("set -e")
print("export DEBIAN_FRONTEND=noninteractive")
print("apt-get update -q")
for c in cmds:
    c = c.replace("@RUNNER_VERSION@", sys.argv[2]).replace("@RUNNER_MIRROR@", "")
    print("bash -c " + shlex.quote(c))
# As hack/aws: Docker's containers can't reach the instance metadata.
print("""mkdir -p /etc/systemd/system/docker.service.d
printf '[Service]\\nExecStartPost=/usr/sbin/iptables -I DOCKER-USER -d 169.254.169.254 -j DROP\\n' > /etc/systemd/system/docker.service.d/no-imds.conf
echo FLEETCI-BAKE-OK
# A fresh cloud-init on each VM made from this image, then stop.
systemd-run --on-active=20s /bin/sh -c 'cloud-init clean --logs --machine-id; poweroff'""")
PY

ami_src=$(aws ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id --query Parameter.Value --output text)
id=$(aws ec2 run-instances --image-id "$ami_src" --instance-type c8i.large \
  --user-data "file://$work/user-data" \
  --instance-initiated-shutdown-behavior stop \
  --metadata-options HttpTokens=required,HttpPutResponseHopLimit=1 \
  --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=30,VolumeType=gp3,Encrypted=true}' \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=fleetci-measure-bake},{Key=Project,Value=fleetci-measure}]' \
  --query 'Instances[0].InstanceId' --output text)
trap 'rm -rf "$work"; aws ec2 terminate-instances --instance-ids "$id" >/dev/null; echo "terminated $id" >&2' EXIT
echo "baking on $id from $ami_src" >&2

# Up to 40 minutes for the recipe.
i=0
while [ "$(aws ec2 describe-instances --instance-ids "$id" --query 'Reservations[0].Instances[0].State.Name' --output text)" != stopped ]; do
  i=$((i + 1))
  [ $i -le 80 ] || { echo "not stopped after 40 minutes" >&2; exit 1; }
  timeout 30 tail -f /dev/null || true
done

# The console keeps the last 64 KB; the marker is near its end.
if ! aws ec2 get-console-output --instance-id "$id" --latest --output text | grep -q FLEETCI-BAKE-OK; then
  echo "the recipe failed; its output:" >&2
  aws ec2 get-console-output --instance-id "$id" --latest --output text | tail -40 >&2
  exit 1
fi

ami=$(aws ec2 create-image --instance-id "$id" --name "fleetci-measure-vm-$(date -u +%Y%m%d-%H%M)" \
  --description "fleetci measurement baseline: images/github.json recipe, actions/runner $runner" \
  --tag-specifications 'ResourceType=image,Tags=[{Key=Project,Value=fleetci-measure}]' 'ResourceType=snapshot,Tags=[{Key=Project,Value=fleetci-measure}]' \
  --query ImageId --output text)
echo "waiting for $ami" >&2
aws ec2 wait image-available --image-ids "$ami" || aws ec2 wait image-available --image-ids "$ami"
echo "$ami"
