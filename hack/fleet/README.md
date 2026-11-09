# The fleet kit

A fleet on one machine, in containers: two controllers and two workers,
each worker on a network of its own, as if at another site. It runs the
same commands a real install does: `fleetci init`, `fleetci token create`,
`fleetci join`, `fleetci controller`, `fleetci worker`.

| container | role | network | budget | classes it gets |
|---|---|---|---|---|
| `fleetci-fleet-ctl-1` | controller, leads | all | – | – |
| `fleetci-fleet-ctl-2` | controller, standby | all | – | – |
| `fleetci-fleet-w1` | worker, `site=1` | `fleetci-fleet-w1` | 2 cpus 4GB | 1vcpu-2gb |
| `fleetci-fleet-w2` | worker, `site=2` | `fleetci-fleet-w2` | 5 cpus 9GB | 1vcpu-2gb, 4vcpu-8gb |
| `fleetci-fleet-w3` | worker without /dev/kvm (`FLEETCI_FLEET_W3=1`) | `fleetci-fleet-w3` | 2 cpus 4GB | none: turned away |

The classes are defined once, in the controllers' `controller.toml`; each
worker runs those its budget fits.

## Up and down

```sh
hack/fleet/up.sh                          # GitHub only
FLEETCI_FLEET_GITLAB=1 hack/fleet/up.sh   # and GitLab: docker stop fleetci-local first
hack/fleet/down.sh                        # FLEETCI_FLEET_PURGE=1 also removes the data
```

It needs `/dev/kvm`, the fleetingd fork checked out next to fleetci, and in
`~/.config/fleetci`: the WireZone GitHub App (`github-app-wirezone.pem`,
`.id`) and, for GitLab, `runner.token`. Only the controllers mount them;
the workers never see a forge credential.

Data lives in `~/.local/share/fleetci-fleet/<machine>`. Up again, a joined
worker runs `fleetci worker` with its `worker.toml` instead of joining.

## Trying it

```sh
alias fleet='docker exec fleetci-fleet-ctl-1 /opt/fleetci/bin/fleetci fleet'
fleet status

gh workflow run basic.yml -R WireZone/fleetci-playground   # one job per class
gh workflow run burst.yml -R WireZone/fleetci-playground   # 4 jobs: w1 fills, then w2
docker logs -f fleetci-fleet-ctl-1                         # "vm placed" per job

docker stop fleetci-fleet-ctl-1      # ctl-2 leads within ~10 s; running jobs go on
docker start fleetci-fleet-ctl-1     # and ctl-1 takes back the lead

fleet drain w1                       # no new jobs on w1; its running ones finish
fleet undrain w1
```

GitLab, with `FLEETCI_FLEET_GITLAB=1`: run a pipeline on
gitlab.com/Nesty92/fleetci-playground. The leader takes the job with the
runner token and the worker runs it with the job's own token.
