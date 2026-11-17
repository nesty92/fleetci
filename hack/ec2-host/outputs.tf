output "host" {
  description = "ssh ubuntu@<this>"
  value       = aws_instance.host.public_ip
}

output "host_instance_id" {
  value = aws_instance.host.id
}

output "vm_groups" {
  value = { for k, g in aws_autoscaling_group.vm : k => g.name }
}
