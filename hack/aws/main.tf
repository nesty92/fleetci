# Two Auto Scaling groups, one per class, for testing fleetci's routing on
# AWS. gitlab-runner runs outside AWS and reaches the VMs on their public
# addresses; only operator_cidr may connect.

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

data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

# VMs accept SSH from the operator only. They cannot reach each other: the
# group has no rule that admits its own members.
resource "aws_security_group" "vm" {
  name_prefix = "fleetci-test-vm-"
  description = "fleetci test VMs: SSH from the operator only"
  vpc_id      = data.aws_vpc.default.id

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "ssh" {
  security_group_id = aws_security_group.vm.id
  description       = "SSH from the machine running gitlab-runner"
  cidr_ipv4         = var.operator_cidr
  ip_protocol       = "tcp"
  from_port         = 22
  to_port           = 22
}

resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.vm.id
  description       = "Image pulls, GitLab, cache and artifacts"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_launch_template" "vm" {
  name_prefix = "fleetci-test-vm-"
  image_id    = data.aws_ssm_parameter.al2023.value
  user_data   = filebase64("${path.module}/user-data.sh")

  vpc_security_group_ids = [aws_security_group.vm.id]

  # No instance profile: a job VM holds no AWS credentials. IMDS stays on
  # for EC2 Instance Connect, with tokens required and one hop, so
  # containers on Docker's bridge cannot reach it; user-data also blocks it
  # for host-network containers.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_size           = 20
      volume_type           = "gp3"
      delete_on_termination = true
      encrypted             = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name = "fleetci-test-vm"
    }
  }
}

# As fleeting-plugin-aws asks: desired and min 0, no scaling policy,
# AZRebalance suspended, scale-in protection on.
resource "aws_autoscaling_group" "class" {
  for_each = var.classes

  name                  = "fleetci-test-${each.key}"
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
      dynamic "override" {
        for_each = each.value.instance_types
        content {
          instance_type = override.value
        }
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
