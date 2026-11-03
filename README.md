# fleetci

Per-job machine classes for GitLab CI, on top of
[fleeting](https://gitlab.com/gitlab-org/fleeting). A job names the size it
needs; it gets a fresh VM of that size, used once.

```yaml
build:
  variables:
    FLEETCI_CLASS: large
  image: golang:1.27
  services: [postgres:17]
  script: go build ./...
```

Stock gitlab-runner does the job as usual (`image:`, `services:`, cache,
artifacts). fleetci only decides where it runs: gitlab-runner loads
`fleeting-plugin-fleetci`, and its `vm_isolation` asks the plugin for a VM
named `class-${FLEETCI_CLASS}` once per job. Each class is its own fleeting
plugin, such as a cloud's, with its own warm VMs and limits.

Status: proof of concept, running locally. See [hack/m0](hack/m0/README.md)
to try it on one machine.
