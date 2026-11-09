# Bare metal with cloud spikes

Your own server runs the steady load as microVMs. When all of a class's
microVMs are busy, its next jobs go to a cloud: AWS, Google Cloud, Azure, or
a different one per class. Jobs keep asking for the same class; they never
name the server or the cloud.

```
dedicated server (OVH, Hetzner, …; KVM)
  gitlab-runner ─ fleeting-plugin-fleetci          GitHub jobs too, from [github]
    class 4vcpu-8gb    fleetingd ─ Cloud Hypervisor microVMs, <1 s from a snapshot
      overflow ──────► class 4vcpu-8gb-aws  fleeting-plugin-aws ─ Auto Scaling group
    class 2vcpu-4gb    fleetingd ─ microVMs
      overflow ──────► class 2vcpu-4gb-gcp  fleeting-plugin-googlecloud ─ managed instance group
```

Needs fleetci 0.4.0 or later (`overflow`) and the fleetingd fork
[v0.4.0-fleetci.2](https://github.com/nesty92/fleeting-plugin-fleetingd/releases/tag/v0.4.0-fleetci.2)
or later. The AWS plugin is tried with fleetci; the Google Cloud and Azure
plugins are not yet, so try them on a test class first.

## How overflow behaves

- A class with `overflow` sends a job to its overflow class while all of
  its own `max_instances` are in use.
- Once a job on the server ends, the next job goes to the server again.
- When both are full, the job takes the first free VM in either, its own
  class first. It waits up to `acquire_timeout`, 50 minutes by default for
  a class with an overflow. On GitLab the job shows as running meanwhile;
  on GitHub it stays queued.
- Each class has at most one overflow, and an overflow class can't have one
  of its own. Different classes can spill to different clouds, which is how
  you mix clouds. One class can't spill to AWS and then to Google Cloud.
- The overflow class is a full class: jobs can ask for it by name, for
  work that must run in that cloud.

## 1. The server

Any dedicated server with hardware virtualisation; Ubuntu 24.04 here. One
server runs one fleetci. For several servers, see
[Several servers](#several-servers).

```sh
ls -l /dev/kvm                     # must exist
apt-get install -y ca-certificates curl nftables qemu-utils
echo net.ipv4.ip_forward=1 > /etc/sysctl.d/90-fleetci.conf && sysctl --system
ip route get 1.1.1.1               # "dev eno1": the egress interface for fleetingd
```

Install the binaries:

```sh
# Cloud Hypervisor
curl -fsSL -o /usr/local/bin/cloud-hypervisor \
  https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v54.0/cloud-hypervisor-static
chmod 755 /usr/local/bin/cloud-hypervisor

# fleetingd, our fork
curl -fsSL https://github.com/nesty92/fleeting-plugin-fleetingd/releases/download/v0.4.0-fleetci.2/fleeting-plugin-fleetingd_linux_amd64.tar.gz \
  | tar -xz -C /usr/local/bin fleeting-plugin-fleetingd

# fleetci and its gitlab-runner plugin
v=0.6.0 arch=linux-amd64 # x-release-please-version
base=https://github.com/nesty92/fleetci/releases/download/v$v
curl -fsSLO "$base/fleetci-$arch" -fsSLO "$base/fleeting-plugin-fleetci-$arch" -fsSLO "$base/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
install -m 755 "fleetci-$arch" /usr/local/bin/fleetci
install -m 755 "fleeting-plugin-fleetci-$arch" /usr/local/bin/fleeting-plugin-fleetci

# gitlab-runner: https://docs.gitlab.com/runner/install/linux-repository/
```

The cloud plugins, only those you use:

```sh
get() { curl -fsSL -o "/usr/local/bin/$1" "$2" && chmod 755 "/usr/local/bin/$1"; }
get fleeting-plugin-aws \
  https://gitlab.com/api/v4/projects/39457165/packages/generic/releases/v1.3.1/fleeting-plugin-aws-linux-amd64
get fleeting-plugin-googlecloud \
  https://gitlab.com/api/v4/projects/39455486/packages/generic/releases/v1.3.1/fleeting-plugin-googlecloud-linux-amd64
get fleeting-plugin-azure \
  https://gitlab.com/api/v4/projects/41087686/packages/generic/releases/v1.2.0/fleeting-plugin-azure-linux-amd64
```

gitlab-runner runs as root here: fleetingd creates tap devices and nftables
rules. Those live in the tables fleetingd owns; the server's own firewall
is left alone.

**Sizing.** Give the microVM classes about the server's cores in vCPUs,
for example 8 × `4vcpu-8gb` on 32 cores, and keep a quarter of the memory
free. Restored microVMs share their snapshot's memory until they write to
it, so idle ones cost little.

## 2. The clouds

Each overflow class needs one instance group, created empty, that only
fleetci scales. The VM image needs Docker, an SSH user in the `docker`
group and, for GitHub jobs, libicu and actions/runner in
`~/actions-runner-<version>`. fleetci waits for cloud-init and Docker before
it hands a VM to a job.

fleetci reaches the cloud VMs from the server over SSH at their public
address (`use_external_addr = true`). Allow SSH (22) into the group only
from the server's IP, or connect the networks with WireGuard or a VPN and
use the private addresses instead.

### AWS

- An Auto Scaling group: min and desired 0, max at least the class's
  `max_instances`, scale-in protection on, the `AZRebalance` process
  suspended. Spot works.
- A launch template with the image and a security group that lets the
  server in. [hack/aws](../../hack/aws) has a working one in terraform.
- On the server, credentials for fleeting-plugin-aws's
  [IAM policy](https://gitlab.com/gitlab-org/fleeting/plugins/aws#recommended-iam-policy),
  scoped to the group. The server is outside AWS, so use
  [IAM Roles Anywhere](https://docs.aws.amazon.com/rolesanywhere/latest/userguide/introduction.html)
  or an access key in a credentials file named by `credentials_file`.

```toml
[classes.4vcpu-8gb-aws]
plugin = "fleeting-plugin-aws"
max_instances = 20
plugin_config = { name = "fleetci-4vcpu-8gb", region = "eu-west-3", credentials_file = "/etc/fleetci/aws-credentials" }
connector_config = { username = "ubuntu", use_external_addr = true }
```

### Google Cloud

- A zonal or regional managed instance group (stateless): autoscaling
  configuration deleted, size 0, default action on failure "No action".
- On the server, a service account key for the plugin, with the role
  `iam.serviceAccountUser` and the permissions
  `compute.instanceGroupManagers.get`, `compute.instanceGroupManagers.update`,
  `compute.instances.get` and `compute.instances.setMetadata`. The plugin puts
  a new SSH key in the instance's metadata for each connection.

```toml
[classes.2vcpu-4gb-gcp]
plugin = "fleeting-plugin-googlecloud"
max_instances = 20
plugin_config = { name = "fleetci-2vcpu-4gb", project = "my-project", zone = "europe-west1-b", credentials_file = "/etc/fleetci/gcp-key.json" }
connector_config = { username = "fleeting", use_external_addr = true }
```

### Azure

- A virtual machine scale set: scaling policy manual, capacity 0, and
  overprovisioning off (`overprovision = false`). With overprovisioning on,
  Azure deletes extra VMs that already have jobs.
- An SSH public key in the scale set's OS profile, and its private key on
  the server: the plugin doesn't hand out keys, so the class logs in with
  this one.
- A service principal that can manage the scale set, given to the plugin
  as `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`, `AZURE_TENANT_ID` and
  `AZURE_SUBSCRIPTION_ID` in gitlab-runner's environment (a systemd
  drop-in, below).

```toml
[classes.8vcpu-16gb-azure]
plugin = "fleeting-plugin-azure"
max_instances = 10
plugin_config = { name = "fleetci-8vcpu-16gb", subscription_id = "…", resource_group_name = "fleetci" }
connector_config = { username = "azureuser", key_path = "/etc/fleetci/azure-ssh-key", use_static_credentials = true, use_external_addr = true }
```

```ini
# /etc/systemd/system/gitlab-runner.service.d/azure.conf
[Service]
EnvironmentFile=/etc/fleetci/azure.env
```

## 3. fleetci.toml

`/etc/fleetci/fleetci.toml`. Each microVM class gets its own group name,
subnet and disk directory. The image is built once per server, and each VM
size gets a snapshot.

```toml
default_class = "2vcpu-4gb"

[classes.2vcpu-4gb]
plugin = "fleeting-plugin-fleetingd"
max_instances = 8
overflow = "2vcpu-4gb-gcp"
connector_config = { username = "ubuntu", timeout = "5m" }

[classes.2vcpu-4gb.plugin_config]
name = "metal2"
egress_interface = "eno1"
vm_subnet = "172.16.121."
vm_disk_directory = "/var/lib/fleetci/vms-metal2"
vm_image_directory = "/var/lib/fleetci/images"
vm_num_cpu_cores = 2
vm_memory_mb = 4096
vm_disk_size_gb = 30
vm_package_upgrade = false
vm_prebuild_on_start = true
vm_snapshot = true
vm_prebuild_cloudinit_extra_cmds = [
  "DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io docker-buildx git zstd",
  "usermod -aG docker ubuntu",
  "su - ubuntu -c 'mkdir -p actions-runner-2.338.2 && curl -fsSL https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-x64-2.338.2.tar.gz | tar -xz -C actions-runner-2.338.2'",
  "/home/ubuntu/actions-runner-2.338.2/bin/installdependencies.sh",
  "systemctl mask apt-daily.timer apt-daily-upgrade.timer chrony-wait.service",
]

[classes.4vcpu-8gb]
plugin = "fleeting-plugin-fleetingd"
max_instances = 6
overflow = "4vcpu-8gb-aws"
connector_config = { username = "ubuntu", timeout = "5m" }

[classes.4vcpu-8gb.plugin_config]
name = "metal4"
egress_interface = "eno1"
vm_subnet = "172.16.120."
vm_disk_directory = "/var/lib/fleetci/vms-metal4"
vm_image_directory = "/var/lib/fleetci/images"
vm_num_cpu_cores = 4
vm_memory_mb = 8192
vm_disk_size_gb = 30
vm_package_upgrade = false
vm_prebuild_on_start = true
vm_snapshot = true
vm_prebuild_cloudinit_extra_cmds = [ "…the same commands…" ]

# the cloud classes from step 2: 2vcpu-4gb-gcp, 4vcpu-8gb-aws, …

[github]                             # optional; see gitlab-and-github.md
repos = ["owner/name"]
token_file = "/etc/fleetci/github.token"
```

In place of the commands, `vm_image_url` can name fleetci's [small
image](../../images), which holds the same software and is rebuilt weekly:
nothing is built on the server, and a new image is downloaded on start.

Both classes share the server's CPUs and memory. fleetci detects them at
start and keeps 1 CPU and a tenth of the memory for the host, then gives a
job a VM only while its size still fits, so `max_instances` (8 and 6 here)
is each class's ceiling, not a share kept for it. A job that doesn't fit
goes to its `overflow`. Without one it waits; jobs that fit go before it
until it has waited 10 minutes, then the server holds the room that frees
for it (`nominate_after`). `[host]` changes the reserve or sets the budget
outright; see the README's
[Sharing this machine's CPUs and memory](../../README.md#sharing-this-machines-cpus-and-memory).

The image commands are cloud-init `runcmd` entries: keep them free of `: `
and ` #`. The names `2vcpu-4gb` and `4vcpu-8gb` are on fleetci's public GitLab
template, so projects need nothing but the template; the cloud classes
aren't, which only matters for jobs that ask for them by name
(`fleetci ci-include -extra`).

## 4. gitlab-runner and the watchdog

Register a runner in GitLab with the tag `fleetci` and "Run untagged jobs"
off, keep its token in `/etc/fleetci/runner.token`, then:

```sh
fleetci runner-config -global -token-file /etc/fleetci/runner.token > /etc/gitlab-runner/config.toml
fleetci check
systemctl restart gitlab-runner
```

gitlab-runner never starts its fleeting plugin again once it dies, so run
fleetci's watchdog beside it:

```ini
# /etc/systemd/system/fleetci-watchdog.service
[Unit]
Description=Restart gitlab-runner when its fleetci plugin is gone
After=gitlab-runner.service

[Service]
ExecStart=/usr/local/bin/fleetci watchdog -plugin fleeting-plugin-fleetci -restart "systemctl restart gitlab-runner"
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload && systemctl enable --now fleetci-watchdog
```

## 5. Check it

`fleetci check` must pass every line: each class's plugin starts and finds its
group, and the GitHub token works. Its `host` line is the server's budget:

```
ok    host       budget cpus=31 memory=113GB (detected 32 cpus, reserve 1; detected 125GB memory, reserve 12GB) for 2vcpu-4gb, 4vcpu-8gb; fixed shares none; nominate_after 10m0s; placement pack
```
 Then run more jobs of one class at once
than the server holds, for example ten `4vcpu-8gb` jobs with
`max_instances = 6`:

```sh
journalctl -u gitlab-runner -f | grep -E 'vm acquired|vm released'
```

```
vm acquired  vm=fleetci-4vcpu-8gb-7b5adaca     class=4vcpu-8gb     waited=0.6s
vm acquired  vm=fleetci-4vcpu-8gb-aws-5c03c696 class=4vcpu-8gb-aws overflow_of=4vcpu-8gb waited=48s
```

As many as the server holds land on it, up to the six of `max_instances`,
and the rest go to the cloud. The first VM of
each size waits a few minutes once, while the server builds the image and
then the snapshot.

## Operating it

| What | How |
| --- | --- |
| How often you spill | count `overflow_of=` lines. Spilling every day means another server would pay off. |
| Jobs wait although their class has room | the server's budget is full: a job without `overflow` that times out says "none of its hosts has room for one more of its VMs" with what is in use on each, and "as room is held for a … job" when a nominated job waited first. Add an `overflow`, or raise the budget with `[host]` if the server has more to give. |
| A class's plugin dies | fleetci starts it again within a second; its running jobs fail and are logged. Nothing to do. |
| fleetci's plugin dies | the watchdog restarts gitlab-runner after 30 s. Jobs running at that moment fail. |
| Cloud VMs left behind | fleetci removes what's left in its groups when it stops and when it starts again; nothing else may scale those groups. |
| Upgrade fleetci | replace the two binaries, `fleetci check`, `systemctl restart gitlab-runner`. Running jobs fail, so do it while quiet. |
| Upgrade fleetingd | replace the binary and restart gitlab-runner; a new version may build a new snapshot (about 12 s). |
| Move or resize disks | the fork rebuilds snapshots whose paths changed; the image stays. |
| The server is down | nothing runs, not even the overflow: fleetci runs on the server. For more than one server, see below. |

## Several servers

fleetingd starts VMs on its own machine only, so each server runs its own
gitlab-runner and fleetci, registered with the same tag `fleetci` and the
same class names. GitLab hands each job to a server with room; if one
server fails, the others keep working.

- Give `overflow` to the last server only, with its own cloud groups. The
  others then take jobs up to their own capacity and leave the spikes to it.
  It isn't exact: GitLab doesn't know when all servers are full.
- Put `[github]` on one server only. Two fleetci processes serving the same
  repos would each start a runner for the same queued job.
- Cloud groups belong to one server: two fleetci processes on one group
  remove each other's VMs.
