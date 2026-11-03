# M0: prove the nesting hook on one machine

M0 shows that stock gitlab-runner can ask fleetci for a machine per job and
still do everything else itself: `image:`, `services:`, the helper
container, cache and artifacts.

```
gitlab-runner (docker-autoscaler, vm_isolation)
  └─ fleeting-plugin-static ─ SSH ─► gateway container (sshd only)
        └─ nesting socket ─► fleetci-nesting (on this host)
              Create("class-large") → new Docker-in-Docker "VM", 4 CPUs, 6 GiB
  └─ SSH through the gateway ─► VM ─► Docker ─► job containers
```

The job picks its class with one variable:

```yaml
build:
  tags: [fleetci]
  variables:
    FLEETCI_CLASS: large   # unset → default class; unknown → system failure
```

## Run it

```sh
hack/m0/up.sh      # images, network, keys, gateway, config.toml
# then, in two terminals, the two commands up.sh prints
hack/m0/down.sh    # remove containers and network
```

`up.sh` reads the runner token from `~/.config/fleetci/m0-runner.token` and
writes it only into `~/.local/share/fleetci-m0/config.toml` (mode 0600).

## What M0 found

- `vm_isolation.image` is expanded with the job's variables, so
  `class-${FLEETCI_CLASS}` reaches the daemon as `class-large`.
- `vm_isolation.connector_config.key_path` is ignored: the VM hop
  authenticates with `password` only.
- gitlab-runner 19.5 needs an explicit `timeout` on both connectors.
- A VM goes back to the daemon (`Delete`) as soon as its job ends.
