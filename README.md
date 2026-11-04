# fleetci

Per-job machine classes for GitLab CI, on top of
[fleeting](https://gitlab.com/gitlab-org/fleeting). A job names the size it
needs; it gets a fresh VM of that size, used once.

```yaml
include:
  - local: .gitlab/fleetci.yml   # one hidden job per class

build:
  extends: .fleetci-4vcpu-8gb    # the one line; drop it to go back
  image: golang:1.27
  services: [postgres:17]
  script: go build ./...
```

Stock gitlab-runner does the job as usual (`image:`, `services:`, cache,
artifacts). fleetci only decides where it runs: gitlab-runner loads
`fleeting-plugin-fleetci`, and its `vm_isolation` asks the plugin for a VM
named `class-${FLEETCI_CLASS}` once per job. Each class is its own fleeting
plugin, such as a cloud's, with its own warm VMs and limits.

Name classes after what they give, such as `2vcpu-4gb` or `8vcpu-32gb-arm`,
so a job's class says what it gets without opening the runner's config.

Each `.fleetci-<class>` sets the runner tag and `FLEETCI_CLASS` together;
`.fleetci` alone gets the default class. Both kits write the include to
`fleetci.yml` next to the runner config ([hack/ci-template.sh](hack/ci-template.sh)).
A misspelled class fails when the pipeline is created
(`unknown keys in extends`), before any VM boots. GitLab does not pass a job's
tags to the runner, so the class can't be a tag alone.

Status: proof of concept, running locally. See [hack/m0](hack/m0/README.md)
to try it on one machine.
