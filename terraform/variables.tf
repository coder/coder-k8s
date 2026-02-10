variable "aws_region" {
  description = "AWS region where the EKS cluster and networking resources will be created."
  type        = string
  default     = "eu-central-1"
}

variable "aws_profile" {
  description = "Optional AWS shared config profile used by Terraform. Leave null to use the default AWS credential chain."
  type        = string
  default     = null
}

variable "cluster_name" {
  description = "Name of the EKS cluster."
  type        = string
  default     = "sandbox-eks"
}

variable "cluster_version" {
  description = "Kubernetes version for the EKS control plane."
  type        = string
  default     = "1.35"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC that hosts the EKS cluster."
  type        = string
  default     = "10.0.0.0/16"

  validation {
    # Subnets use netnum up to 20, so we need at least 5 bits (2^5 = 32 > 20).
    # That means VPC prefix + 5 bits <= 24, i.e. prefix <= 19.
    condition     = tonumber(split("/", var.vpc_cidr)[1]) <= 19
    error_message = "VPC CIDR prefix must be /19 or shorter so that /24 subnets with network numbers up to 20 can be allocated."
  }
}

variable "node_instance_types" {
  description = "EC2 instance types for the managed node group."
  type        = list(string)
  default     = ["t3.medium"]
}

variable "node_desired_size" {
  description = "Desired number of worker nodes in the managed node group."
  type        = number
  default     = 2
}

variable "node_min_size" {
  description = "Minimum number of worker nodes in the managed node group."
  type        = number
  default     = 1
}

variable "node_max_size" {
  description = "Maximum number of worker nodes in the managed node group."
  type        = number
  default     = 3
}

variable "node_disk_size" {
  description = "Disk size in GiB for each node in the managed node group."
  type        = number
  default     = 20
}
