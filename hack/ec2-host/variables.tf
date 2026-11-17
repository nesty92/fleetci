variable "region" {
  description = "AWS region. It must have the host's and the VMs' instance types (see the README's check)."
  type        = string
}

variable "profile" {
  description = "AWS CLI profile to use (from ~/.aws/config)."
  type        = string
  default     = null
}

variable "operator_cidr" {
  description = "Only this address range may SSH to the host: your machine, as a /32."
  type        = string

  validation {
    condition     = can(cidrhost(var.operator_cidr, 0)) && !startswith(var.operator_cidr, "0.0.0.0")
    error_message = "operator_cidr must be a real address range such as 203.0.113.7/32, not 0.0.0.0/0."
  }
}

variable "ssh_public_key" {
  description = "Your SSH public key, for the ubuntu user on the host."
  type        = string
}

variable "github_repo" {
  description = "The repository the load runs on, owner/name. fleetci serves its jobs only."
  type        = string
}

variable "fleetci_version" {
  description = "The fleetci release the host runs."
  type        = string
  default     = "0.9.1"
}

variable "host_type" {
  description = "The host: an instance type with nested virtualization (C8i, M8i, R8i and others; see the README)."
  type        = string
  default     = "c8i.4xlarge"
}

variable "host_disk" {
  description = "The host's root disk (gp3), which holds the microVMs' disks, images and cache disks."
  type = object({
    size_gb    = number
    iops       = number
    throughput = number
  })
  default = { size_gb = 200, iops = 3000, throughput = 250 }
}

variable "vm_ami" {
  description = "The baseline VMs' image, made by bake.sh: the same software as the microVMs'."
  type        = string
}

variable "vm_classes" {
  description = "The baseline: one EC2 VM per job, one Auto Scaling group per class. The same family as the host, so the CPUs match."
  type = map(object({
    instance_type = string
    max_instances = number
  }))
  default = {
    "ec2-4vcpu-8gb" = { instance_type = "c8i.xlarge", max_instances = 6 }
    "ec2-2vcpu-4gb" = { instance_type = "c8i.large", max_instances = 6 }
  }
}

variable "vm_disk_gb" {
  description = "Each baseline VM's root disk (gp3)."
  type        = number
  default     = 30
}

variable "use_spot" {
  description = "Run the baseline VMs on Spot. Off: the host is on-demand, so the first comparison is like for like."
  type        = bool
  default     = false
}
