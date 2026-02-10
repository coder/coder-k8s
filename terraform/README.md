# Terraform EKS Sandbox Configuration

This directory provisions a cost-optimized Amazon EKS sandbox cluster in region `eu-central-1`.

## What this sets up

- A VPC (`10.0.0.0/16`) with:
  - 2 public subnets across the first two standard availability zones in the selected region
  - 2 private subnets across the first two standard availability zones in the selected region
  - Internet Gateway
  - Single NAT Gateway (lower cost than one per AZ)
- IAM roles for EKS control plane and worker nodes
- EKS cluster (`sandbox-eks`, Kubernetes `1.31`) with public and private API endpoint access
- One managed node group:
  - Instance type: `t3.medium`
  - Desired/min/max size: `2/1/3`
  - Disk size: `20 GiB`
  - AMI type: `AL2023_x86_64_STANDARD`
- EKS managed add-ons: `coredns`, `kube-proxy`, `vpc-cni`
- Local Terraform state (no remote backend configured)

## Prerequisites

- Terraform `>= 1.5`
- AWS CLI v2 installed
- AWS identity with permissions to create VPC, IAM, EKS, and EC2 resources in your target account

## AWS authentication with auto-refresh

By default, this Terraform config uses the normal AWS SDK credential chain (`aws_profile = null`), so CI/role-based credentials continue to work without local profile setup.

If you use AWS CLI v2 `aws login` (`login_session` profiles) and want automatic refresh during long `terraform apply` runs, use a wrapper profile backed by `credential_process`:

1. Sign in to your normal AWS login profile:

```bash
aws login --profile <your-login-profile>
```

2. Add a Terraform wrapper profile in `~/.aws/config`:

```ini
[profile terraform]
credential_process = aws configure export-credentials --profile <your-login-profile> --format process
region = eu-central-1
```

3. Keep temporary key env vars unset (so Terraform can use profile/SDK auth):

```bash
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
```

4. Verify the wrapper profile works:

```bash
aws sts get-caller-identity --profile terraform
```

5. Set the profile once for all Terraform commands that talk to AWS:

```bash
export TF_VAR_aws_profile=terraform
terraform init
terraform plan
terraform apply
terraform destroy
```

If you prefer not to use `TF_VAR_aws_profile`, pass `-var="aws_profile=<your-terraform-profile>"` to each AWS-authenticated command (`plan`, `apply`, `destroy`).

> Security note: do not commit `~/.aws/config`, `~/.aws/credentials`, or any copied credential values to git.

## Usage

```bash
terraform init
terraform plan
terraform apply
```

## Configure kubectl

After `terraform apply`, run the command from the Terraform output:

```bash
terraform output -raw kubeconfig_command
```

Then execute the printed command, for example:

```bash
aws eks update-kubeconfig --region eu-central-1 --name sandbox-eks
```

## Estimated cost (rough)

- EKS control plane: **~$0.10/hour**
- 2x `t3.medium` worker nodes: **~$0.08/hour**
- 1x NAT Gateway: **~$0.045/hour**
- **Total: ~ $0.225/hour (~$5.40/day)**

> Note: Data transfer and NAT data processing charges are additional.

## Cleanup

```bash
terraform destroy
```
