terraform {
  required_version = ">= 1.8"
  required_providers {
    aws = {
      source = "hashicorp/aws"
      # 6.33.0 added cpu_options.nested_virtualization.
      version = "~> 6.33"
    }
  }
}

provider "aws" {
  region  = var.region
  profile = var.profile

  default_tags {
    tags = {
      Project = "fleetci-measure"
    }
  }
}
