# Runbooks

Step-by-step setups for running fleetci, each with how to check it works,
what to watch and what to do when something breaks.

| Runbook | For |
| --- | --- |
| [Bare metal with cloud spikes](bare-metal-with-cloud-spikes.md) | your own servers (OVH, Hetzner, a rack) for the steady load, and AWS, Google Cloud or Azure for the spikes |
| [GitLab and GitHub from one runner](gitlab-and-github.md) | both forges on the same classes, served from gitlab-runner's fleetci plugin, with no extra process |
| [GitHub only](github-only.md) | GitHub Actions without GitLab: `fleetci serve` |

On AWS alone, [terraform-aws-gitlab-runner-autoscaler](https://github.com/nesty92/terraform-aws-gitlab-runner-autoscaler)
sets everything up from its `fleetci` input; the [README](../../README.md#install)
has the same steps by hand.
