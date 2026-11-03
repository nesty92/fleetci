output "groups" {
  description = "Auto Scaling group per class."
  value       = { for k, g in aws_autoscaling_group.class : k => g.name }
}

output "region" {
  value = var.region
}

output "profile" {
  value = var.profile
}

output "classes" {
  description = "max_instances per class, for the runner config."
  value       = { for k, c in var.classes : k => c.max_instances }
}
