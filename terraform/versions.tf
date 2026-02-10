terraform {
  required_version = ">= 1.14"

  backend "s3" {
    region       = "eu-central-1"
    encrypt      = true
    use_lockfile = true
    # bucket and key supplied via -backend-config at init time.
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

provider "aws" {
  region  = var.aws_region
  profile = var.aws_profile

  default_tags {
    tags = {
      ManagedBy = "terraform"
      Project   = "sandbox-eks"
    }
  }
}
