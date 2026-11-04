# M0: one VM per job, sized by the job, on one machine

Stock gitlab-runner asks fleetci for a VM per job through `vm_isolation`
and does everything else itself: `image:`, `services:`, the helper
container, cache and artifacts.

```
gitlab-runner (docker-autoscaler, vm_isolation)
  │ loads
  ▼
fleeting-plugin-fleetci  ── one process ──────────────────────────────────
  gateway  SSH on 127.0.0.1, accepts only keys it issued (ConnectInfo)
    ├─ nesting socket → Create("class-4vcpu-6gb") → Acquire from that class
    ├─ "exit 0"       → the runner's health check; nothing else runs
    └─ fleetci-vm-…:22 → proxy: ends the runner's VM connection
                              (password "unused": it never leaves the process)
  class "1vcpu-2gb": fleeting plugin + taskscaler, 1 idle VM, max 4
  class "4vcpu-6gb": fleeting plugin + taskscaler, boots per job, max 2
  ─────────────────────────────────────────────────────────────────────────
         └─ SSH with a key the class plugin just issued ─► VM ─► Docker
```

Only the last connection crosses the network. Each class is a fleeting
plugin under its own taskscaler with `capacity_per_instance = 1` and
`max_use_count = 1`: a VM serves one job and is then removed. Here every
class uses `fleeting-plugin-dockervm`, whose instances are local
Docker-in-Docker containers. Like EC2 Instance Connect, it hands out a new
key on every `ConnectInfo` that the VM accepts for two minutes
(`expiry-time` in `authorized_keys`). A cloud class swaps in that cloud's
fleeting plugin; nothing else changes.

The job picks its class with one variable:

```yaml
build:
  tags: [fleetci]
  variables:
    FLEETCI_CLASS: 4vcpu-6gb   # unset → default class; unknown → system failure
```

## Run it

```sh
hack/m0/up.sh      # VM image, network, plugin binaries, config.toml
# then the gitlab-runner command up.sh prints
hack/m0/down.sh    # remove leftover VMs and the network
```

`up.sh` reads the runner token from `~/.config/fleetci/m0-runner.token` and
writes it only into `~/.local/share/fleetci-m0/config.toml` (mode 0600).

## What M0 found

- `vm_isolation.image` is expanded with the job's variables, so
  `class-${FLEETCI_CLASS}` reaches fleetci as `class-4vcpu-6gb`.
- `vm_isolation.connector_config.key_path` is ignored; the runner logs in to
  a VM with `password` only. fleetci ends that connection itself, so the
  password is a placeholder.
- gitlab-runner 19.5 needs an explicit `timeout` on both connectors.
- The runner checks the instance by running `exit 0`, and reaches Docker
  with `docker system dial-stdio` (an exec), so the proxy forwards command
  sessions as well as socket streams.
- A VM goes back to fleetci (`Delete`) as soon as its job ends.
- taskscaler removes on shutdown only the instances it has seen running. A
  VM still booting at that moment is left behind; on the next start it shows
  up as pre-existing and fleetci removes it before any job can use it.

## Docker integration test

```sh
go test -tags docker ./internal/fleet/
```

Starts a class on `fleeting-plugin-dockervm`, acquires a VM, connects twice
with fresh keys, and checks the VM is removed after its one use.
