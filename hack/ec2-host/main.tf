# One host with nested virtualization running fleetci serve, whose classes
# are microVMs on the host (fleetingd) and, for the baseline, one EC2 VM
# per job (fleeting-plugin-aws). Both run the same jobs on the same CPUs.

data "aws_caller_identity" "me" {}

data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
  filter {
    name   = "default-for-az"
    values = ["true"]
  }
}

data "aws_ssm_parameter" "ubuntu" {
  name = "/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
}

# The host: SSH from the operator only.
resource "aws_security_group" "host" {
  name_prefix = "fleetci-measure-host-"
  description = "fleetci measurement host: SSH from the operator only"
  vpc_id      = data.aws_vpc.default.id

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "host_ssh" {
  security_group_id = aws_security_group.host.id
  description       = "SSH from the operator"
  cidr_ipv4         = var.operator_cidr
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
}

resource "aws_vpc_security_group_egress_rule" "host_all" {
  security_group_id = aws_security_group.host.id
  description       = "GitHub, images, packages, the baseline VMs"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

# The baseline VMs: SSH from the host only, which reaches them at their
# private addresses.
resource "aws_security_group" "vm" {
  name_prefix = "fleetci-measure-vm-"
  description = "fleetci measurement VMs: SSH from the host only"
  vpc_id      = data.aws_vpc.default.id

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "vm_ssh" {
  security_group_id            = aws_security_group.vm.id
  description                  = "SSH from the fleetci host"
  referenced_security_group_id = aws_security_group.host.id
  ip_protocol                  = "tcp"
  from_port                    = 22
  to_port                      = 22
}

resource "aws_vpc_security_group_egress_rule" "vm_all" {
  security_group_id = aws_security_group.vm.id
  description       = "GitHub, images, packages"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

# The host's role: fleeting-plugin-aws's policy, for the baseline groups
# only.
resource "aws_iam_role" "host" {
  name_prefix = "fleetci-measure-host-"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "host" {
  role = aws_iam_role.host.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["autoscaling:SetDesiredCapacity", "autoscaling:SetInstanceProtection", "autoscaling:TerminateInstanceInAutoScalingGroup"]
        Resource = [for g in aws_autoscaling_group.vm : g.arn]
      },
      {
        Effect   = "Allow"
        Action   = ["autoscaling:DescribeAutoScalingGroups", "ec2:DescribeInstances", "ec2:DescribeSpotInstanceRequests"]
        Resource = "*"
      },
      {
        Effect    = "Allow"
        Action    = "ec2-instance-connect:SendSSHPublicKey"
        Resource  = "arn:aws:ec2:${var.region}:${data.aws_caller_identity.me.account_id}:instance/*"
        Condition = { StringEquals = { "aws:ResourceTag/fleetci-measure" = "vm" } }
      },
    ]
  })
}

resource "aws_iam_instance_profile" "host" {
  name_prefix = "fleetci-measure-host-"
  role        = aws_iam_role.host.name
}

resource "aws_key_pair" "operator" {
  key_name_prefix = "fleetci-measure-"
  public_key      = var.ssh_public_key
}

resource "aws_instance" "host" {
  ami                    = data.aws_ssm_parameter.ubuntu.value
  instance_type          = var.host_type
  subnet_id              = data.aws_subnets.default.ids[0]
  vpc_security_group_ids = [aws_security_group.host.id]
  iam_instance_profile   = aws_iam_instance_profile.host.name
  key_name               = aws_key_pair.operator.key_name
  user_data = templatefile("${path.module}/host-user-data.sh", {
    fleetci_version = var.fleetci_version
    github_repo     = var.github_repo
    region          = var.region
    vm_classes      = var.vm_classes
  })
  user_data_replace_on_change = true

  cpu_options {
    nested_virtualization = "enabled"
  }

  # Tokens required, one hop: the microVMs, a hop further, can't read the
  # host's role. user-data also drops their traffic to it.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    volume_size = var.host_disk.size_gb
    volume_type = "gp3"
    iops        = var.host_disk.iops
    throughput  = var.host_disk.throughput
    encrypted   = true
  }

  tags = {
    Name = "fleetci-measure-host"
  }
}

resource "aws_launch_template" "vm" {
  name_prefix = "fleetci-measure-vm-"
  image_id    = var.vm_ami

  vpc_security_group_ids = [aws_security_group.vm.id]

  # No instance profile: a job VM holds no AWS credentials. IMDS stays on
  # for EC2 Instance Connect, tokens required, one hop.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      volume_size           = var.vm_disk_gb
      volume_type           = "gp3"
      delete_on_termination = true
      encrypted             = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name            = "fleetci-measure-vm"
      fleetci-measure = "vm"
    }
  }
}

# As fleeting-plugin-aws asks: desired and min 0, no scaling policy,
# AZRebalance suspended, scale-in protection on.
resource "aws_autoscaling_group" "vm" {
  for_each = var.vm_classes

  name                  = "fleetci-measure-${each.key}"
  min_size              = 0
  max_size              = each.value.max_instances
  desired_capacity      = 0
  vpc_zone_identifier   = data.aws_subnets.default.ids
  protect_from_scale_in = true
  suspended_processes   = ["AZRebalance"]

  mixed_instances_policy {
    instances_distribution {
      on_demand_base_capacity                  = 0
      on_demand_percentage_above_base_capacity = var.use_spot ? 0 : 100
      spot_allocation_strategy                 = "price-capacity-optimized"
    }
    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.vm.id
        version            = aws_launch_template.vm.latest_version
      }
      override {
        instance_type = each.value.instance_type
      }
    }
  }

  tag {
    key                 = "fleetci-class"
    value               = each.key
    propagate_at_launch = true
  }

  # fleeting owns the size from here on.
  lifecycle {
    ignore_changes = [desired_capacity]
  }
}
