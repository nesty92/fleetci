# AWS test: two classes, two Auto Scaling groups

This tests fleetci's routing on AWS. Each class is an Auto Scaling group
that `fleeting-plugin-aws` scales, and is named after what it gives:
`2vcpu-1gb` is `t3.micro` and `2vcpu-2gb` is `t3.small`, both on the AWS
free plan. A job with `FLEETCI_CLASS: 2vcpu-2gb` must land in
`fleetci-test-2vcpu-2gb`.

```
your machine                                     AWS (default VPC)
gitlab-runner ─ fleeting-plugin-fleetci          ASG fleetci-test-2vcpu-1gb (t3.micro)
                  ├─ class 2vcpu-1gb ─ fleeting-plugin-aws ─► ASG fleetci-test-2vcpu-2gb (t3.small)
                  └─ class 2vcpu-2gb ─ fleeting-plugin-aws
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
