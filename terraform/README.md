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
- Remote Terraform state in AWS S3 with lockfile-based locking (no DynamoDB)

## Prerequisites

- Terraform `>= 1.11`
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

## Remote state backend (S3 + lockfile)

Terraform state is stored in an S3 bucket in `eu-central-1`. Locking uses Terraform's native S3 lockfile (`use_lockfile = true` in the backend block), so no DynamoDB lock table is required. The backend configuration committed in this repo only includes shared settings (`region`, `encrypt`, and `use_lockfile`); the bucket name and state key are supplied at init time via `-backend-config`.

### Bootstrap the S3 bucket (one-time)

Terraform cannot create its own backend bucket before backend initialization, so create and secure the bucket once with AWS CLI:

```bash
REGION=eu-central-1
BUCKET=<globally-unique-bucket-name>

# Create bucket
aws s3api create-bucket \
  --bucket "$BUCKET" \
  --region "$REGION" \
  --create-bucket-configuration LocationConstraint="$REGION"

# Enable versioning (for state recovery)
aws s3api put-bucket-versioning \
  --bucket "$BUCKET" \
  --versioning-configuration Status=Enabled

# Block all public access
aws s3api put-public-access-block \
  --bucket "$BUCKET" \
  --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true

# Default encryption (SSE-S3)
aws s3api put-bucket-encryption \
  --bucket "$BUCKET" \
  --server-side-encryption-configuration '{
    "Rules": [{
      "ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "AES256"}
    }]
  }'
```

### Required IAM permissions

The identity used for Terraform backend access needs S3 permissions for `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` (lockfile lifecycle), and `s3:ListBucket` on the state bucket path.

### Authentication note

The S3 backend authenticates separately from the AWS provider configuration. If you use `TF_VAR_aws_profile` for provider auth, also set `AWS_PROFILE` or add `profile` to `backend.hcl` so `terraform init` can authenticate to S3.

### Migrate existing local state

If you already have a local `.tfstate` file, migrate it into S3 during init:

```bash
terraform init -migrate-state -backend-config=backend.hcl
```

## Usage

Create a local backend config file `backend.hcl` in this directory (it is
git-ignored):

```hcl
bucket = "<your-tfstate-bucket>"
key    = "terraform-ncp3/sandbox-eks/terraform.tfstate"

# Optional: set if you use an AWS CLI profile for backend access
# profile = "terraform"
```

Then initialize and apply:

```bash
terraform init -backend-config=backend.hcl
terraform plan
terraform apply
```

## Importing existing AWS resources

Use this flow when your remote state is empty but AWS infrastructure already exists.

- Do **not** run `terraform apply` until imports are complete and `terraform plan` is clean.
- Ensure Terraform variables match the values used when the infrastructure was originally created.

<details>
<summary>Discover resource IDs with AWS CLI</summary>

```bash
REGION=eu-central-1
CLUSTER=sandbox-eks

# VPC
aws ec2 describe-vpcs --region "$REGION" \
  --filters "Name=tag:Name,Values=${CLUSTER}-vpc" \
  --query 'Vpcs[0].VpcId' --output text

# Internet Gateway
aws ec2 describe-internet-gateways --region "$REGION" \
  --filters "Name=tag:Name,Values=${CLUSTER}-igw" \
  --query 'InternetGateways[0].InternetGatewayId' --output text

# Subnets (public + private)
aws ec2 describe-subnets --region "$REGION" \
  --filters "Name=tag:Name,Values=${CLUSTER}-public-*" \
  --query 'Subnets[].{id:SubnetId,az:AvailabilityZone,name:Tags[?Key==`Name`]|[0].Value}' --output table

aws ec2 describe-subnets --region "$REGION" \
  --filters "Name=tag:Name,Values=${CLUSTER}-private-*" \
  --query 'Subnets[].{id:SubnetId,az:AvailabilityZone,name:Tags[?Key==`Name`]|[0].Value}' --output table

# EIP, NAT Gateway, Route Tables
aws ec2 describe-addresses --region "$REGION" \
  --filters "Name=tag:Name,Values=${CLUSTER}-nat-eip" \
  --query 'Addresses[0].AllocationId' --output text

aws ec2 describe-nat-gateways --region "$REGION" \
  --filter "Name=tag:Name,Values=${CLUSTER}-nat" \
  --query 'NatGateways[0].NatGatewayId' --output text

aws ec2 describe-route-tables --region "$REGION" \
  --filters "Name=tag:Name,Values=${CLUSTER}-public-rt" \
  --query 'RouteTables[0].RouteTableId' --output text

aws ec2 describe-route-tables --region "$REGION" \
  --filters "Name=tag:Name,Values=${CLUSTER}-private-rt" \
  --query 'RouteTables[0].RouteTableId' --output text

# EKS cluster, node groups, addons
aws eks describe-cluster --region "$REGION" --name "$CLUSTER" \
  --query 'cluster.name' --output text

aws eks list-nodegroups --region "$REGION" --cluster-name "$CLUSTER"
aws eks list-addons --region "$REGION" --cluster-name "$CLUSTER"

# IAM roles
aws iam get-role --role-name "${CLUSTER}-cluster-role" --query 'Role.RoleName' --output text
aws iam get-role --role-name "${CLUSTER}-node-role" --query 'Role.RoleName' --output text
```

</details>

<details>
<summary>Import commands by Terraform file</summary>

### Networking (`vpc.tf`)

```bash
terraform import aws_vpc.this <vpc-id>
terraform import aws_internet_gateway.this <igw-id>
terraform import 'aws_subnet.public[0]' <subnet-id>
terraform import 'aws_subnet.public[1]' <subnet-id>
terraform import 'aws_subnet.private[0]' <subnet-id>
terraform import 'aws_subnet.private[1]' <subnet-id>
terraform import aws_eip.nat <eipalloc-id>
terraform import aws_nat_gateway.this <nat-id>
terraform import aws_route_table.public <rtb-id>
terraform import aws_route_table.private <rtb-id>
terraform import 'aws_route_table_association.public[0]' <subnet-id>/<rtb-id>
terraform import 'aws_route_table_association.public[1]' <subnet-id>/<rtb-id>
terraform import 'aws_route_table_association.private[0]' <subnet-id>/<rtb-id>
terraform import 'aws_route_table_association.private[1]' <subnet-id>/<rtb-id>
```

### IAM (`iam.tf`)

```bash
terraform import aws_iam_role.eks_cluster sandbox-eks-cluster-role
terraform import aws_iam_role.node_group sandbox-eks-node-role
terraform import aws_iam_role_policy_attachment.eks_cluster_policy \
  'sandbox-eks-cluster-role/arn:aws:iam::aws:policy/AmazonEKSClusterPolicy'
terraform import aws_iam_role_policy_attachment.node_group_worker_policy \
  'sandbox-eks-node-role/arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy'
terraform import aws_iam_role_policy_attachment.node_group_cni_policy \
  'sandbox-eks-node-role/arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy'
terraform import aws_iam_role_policy_attachment.node_group_ecr_readonly \
  'sandbox-eks-node-role/arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly'
```

### EKS (`eks.tf`)

```bash
terraform import aws_eks_cluster.this sandbox-eks
terraform import aws_eks_node_group.this sandbox-eks:sandbox-eks-nodes
terraform import aws_eks_addon.coredns sandbox-eks:coredns
terraform import aws_eks_addon.kube_proxy sandbox-eks:kube-proxy
terraform import aws_eks_addon.vpc_cni sandbox-eks:vpc-cni
```

</details>

After importing, run `terraform plan` and confirm the result is ideally a no-op.

Common post-import issues:

- Subnet index/AZ mismatch can cause Terraform to propose subnet recreation; fix by swapping imported subnet indices (`public[0]`/`public[1]`, `private[0]`/`private[1]`) to match Terraform's AZ ordering.
- EKS add-on configuration drift may still appear depending on AWS-managed defaults; review and reconcile intentionally.

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
