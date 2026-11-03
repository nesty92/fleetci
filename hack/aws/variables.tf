variable "region" {
  description = "AWS region for the test."
  type        = string
}

variable "profile" {
  description = "AWS CLI profile to use (from ~/.aws/config)."
  type        = string
  default     = null
}

variable "operator_cidr" {
  description = "Only this address range may SSH to the VMs: the machine running gitlab-runner, as a /32."
  type        = string

  validation {
    condition     = can(cidrhost(var.operator_cidr, 0)) && !startswith(var.operator_cidr, "0.0.0.0")
    error_message = "operator_cidr must be a real address range such as 203.0.113.7/32, not 0.0.0.0/0."
  }
}

variable "classes" {
  description = "One Auto Scaling group per class. The names are what jobs put in FLEETCI_CLASS."
  type = map(object({
    instance_types = list(string)
    max_instances  = number
  }))
  default = {
    small = { instance_types = ["t3.micro"], max_instances = 2 }
    large = { instance_types = ["t3.small"], max_instances = 2 }
  }
}

variable "use_spot" {
  description = "Run the VMs on Spot. Off by default, since the AWS free plan may not allow Spot."
  type        = bool
  default     = false
}
