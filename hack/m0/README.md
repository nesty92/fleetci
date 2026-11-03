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
    ├─ nesting socket → Create("class-large") → new VM: 4 CPUs, 6 GiB
    ├─ "exit 0"       → the runner's health check; nothing else runs
    └─ fleetci-vm-…:22 → proxy: ends the runner's VM connection
                              (password "unused": it never leaves the process)
  ─────────────────────────────────────────────────────────────────────────
         └─ SSH with this VM's own key, host key pinned ─► VM ─► Docker
```

Only the last connection crosses the network. Each VM gets a fresh key and
fresh host keys when it is created, and accepts no passwords.

The job picks its class with one variable:

```yaml
build:
  tags: [fleetci]
  variables:
    FLEETCI_CLASS: large   # unset → default class; unknown → system failure
```

## Run it

```sh
hack/m0/up.sh      # VM image, network, plugin binary, config.toml
# then the gitlab-runner command up.sh prints
hack/m0/down.sh    # remove leftover VMs and the network
```

`up.sh` reads the runner token from `~/.config/fleetci/m0-runner.token` and
writes it only into `~/.local/share/fleetci-m0/config.toml` (mode 0600).

## What M0 found

- `vm_isolation.image` is expanded with the job's variables, so
  `class-${FLEETCI_CLASS}` reaches fleetci as `class-large`.
- `vm_isolation.connector_config.key_path` is ignored; the runner logs in to
  a VM with `password` only. fleetci ends that connection itself, so the
  password is a placeholder.
- gitlab-runner 19.5 needs an explicit `timeout` on both connectors.
- The runner checks the instance by running `exit 0`, and reaches Docker
  with `docker system dial-stdio` (an exec), so the proxy forwards command
  sessions as well as socket streams.
- A VM goes back to fleetci (`Delete`) as soon as its job ends.
