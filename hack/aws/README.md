# AWS test: two classes, two Auto Scaling groups

This tests fleetci's routing on AWS. Each class is an Auto Scaling group
that `fleeting-plugin-aws` scales. `small` uses `t3.micro` and `large` uses
`t3.small`, both on the AWS free plan. The names only matter to routing:
a job with `FLEETCI_CLASS: large` must land in `fleetci-test-large`.

```
your machine                                   AWS (default VPC)
gitlab-runner ─ fleeting-plugin-fleetci        ASG fleetci-test-small (t3.micro)
                  ├─ class small ─ fleeting-plugin-aws ─► ASG fleetci-test-large (t3.small)
                  └─ class large ─ fleeting-plugin-aws
                  proxy ── SSH, 60-second EC2 Instance Connect key ──► VM public IP
```

gitlab-runner runs on your machine and reaches the VMs on their public
addresses. Only `operator_cidr` can open SSH to them. The VMs have no
instance profile, so no AWS credentials.

## What it creates

- one security group: SSH in from `operator_cidr` only, all traffic out;
- one launch template: Amazon Linux 2023 with Docker, IMDSv2 with a hop
  limit of 1, encrypted 20 GiB gp3;
- one Auto Scaling group per class: desired 0, min 0, scale-in protection
  on, `AZRebalance` suspended, as fleeting-plugin-aws asks.

All of it is free while idle. VMs run only while a job runs (`idle_count = 0`
in the runner config). With `use_spot = false` (the default), a `t3.micro`
costs about $0.01 an hour.

## Run it

You need:

- an AWS CLI profile on your personal account (`aws configure sso`);
- the runner token in `~/.config/fleetci/m0-runner.token` (from hack/m0);
- `tofu` and `jq`.

```sh
cd hack/aws
cp terraform.tfvars.example terraform.tfvars   # set region, profile, operator_cidr
tofu init
tofu apply
./runner-config.sh
gitlab-runner run --config ~/.local/share/fleetci-aws/config.toml --working-directory ~/.local/share/fleetci-aws/work
```

Stop the local runner from hack/m0 first, since it uses the same token.

The credentials used by fleeting-plugin-aws need the plugin's IAM policy
(see its README), scoped to the two groups: `autoscaling:SetDesiredCapacity`,
`SetInstanceProtection`, `TerminateInstanceInAutoScalingGroup`,
`DescribeAutoScalingGroups`, `ec2:DescribeInstances`,
`ec2:DescribeSpotInstanceRequests` and `ec2-instance-connect:SendSSHPublicKey`.
An admin profile on a personal test account covers them.

## Clean up

```sh
tofu destroy
```

Stop gitlab-runner first, so it removes its VMs. Any it could not remove are
in the groups, and `tofu destroy` deletes them with the groups.
