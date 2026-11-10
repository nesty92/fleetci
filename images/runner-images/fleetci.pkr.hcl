# The image GitHub's ubuntu-latest runs, built from actions/runner-images
# for fleetci: prepare.sh puts this file beside GitHub's own Ubuntu build,
# whose Azure source it replaces with these two.
#
#   amazon-ebs.image  an AMI, for a class on AWS
#   qemu.image        a qcow2 and its kernel, for a fleetingd class

packer {
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = "~> 1"
    }
    qemu = {
      source  = "github.com/hashicorp/qemu"
      version = "~> 1"
    }
  }
}

variable "fleetci_arch" {
  type        = string
  default     = "amd64"
  description = "amd64 or arm64; qemu builds the host's own, with KVM"
}

variable "fleetci_ubuntu" {
  type        = string
  default     = "24.04"
  description = "the Ubuntu release of the build prepare.sh took"
}

variable "fleetci_codename" {
  type    = string
  default = "noble"
}

variable "fleetci_aws_region" {
  type    = string
  default = "eu-north-1"
}

variable "fleetci_disk_gb" {
  type        = number
  default     = 75
  description = "GitHub builds on 75 GB, and the tools take most of it"
}

source "amazon-ebs" "image" {
  region        = var.fleetci_aws_region
  instance_type = var.fleetci_arch == "arm64" ? "c7g.2xlarge" : "c7i.2xlarge"
  ami_name      = "runner-images-ubuntu-${var.fleetci_ubuntu}-${var.fleetci_arch}-${formatdate("YYYYMMDD-hhmm", timestamp())}"
  source_ami_filter {
    filters = {
      name                = "ubuntu/images/hvm-ssd-gp3/ubuntu-${var.fleetci_codename}-${var.fleetci_ubuntu}-${var.fleetci_arch}-server-*"
      virtualization-type = "hvm"
      root-device-type    = "ebs"
    }
    owners      = ["099720109477"] # Canonical
    most_recent = true
  }
  ssh_username = "ubuntu"
  launch_block_device_mappings {
    device_name           = "/dev/sda1"
    volume_size           = var.fleetci_disk_gb
    volume_type           = "gp3"
    delete_on_termination = true
  }
}

# Ubuntu's cloud image, the one fleetingd builds on, grown to the disk
# GitHub's build needs. The password is the build's alone: the last step
# locks it, and fleetingd's VMs take an SSH key.
source "qemu" "image" {
  iso_url          = "https://cloud-images.ubuntu.com/${var.fleetci_codename}/current/${var.fleetci_codename}-server-cloudimg-${var.fleetci_arch}.img"
  iso_checksum     = "file:https://cloud-images.ubuntu.com/${var.fleetci_codename}/current/SHA256SUMS"
  disk_image       = true
  disk_size        = "${var.fleetci_disk_gb}G"
  format           = "qcow2"
  accelerator      = "kvm"
  cpus             = 4
  memory           = 8192
  headless         = true
  output_directory = "output-fleetingd"
  vm_name          = "runner-images-${var.fleetci_arch}.qcow2"
  cd_label         = "cidata"
  cd_content = {
    "meta-data" = ""
    "user-data" = <<-EOT
      #cloud-config
      password: packer
      chpasswd: { expire: false }
      ssh_pwauth: true
    EOT
  }
  ssh_username     = "ubuntu"
  ssh_password     = "packer"
  ssh_timeout      = "20m"
  shutdown_command = "sudo shutdown -P now"
}
