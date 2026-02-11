output "cluster_name" {
  description = "Name of the EKS cluster."
  value       = aws_eks_cluster.this.name
}

output "cluster_endpoint" {
  description = "API server endpoint for the EKS cluster."
  value       = aws_eks_cluster.this.endpoint
}

output "cluster_certificate_authority" {
  description = "Base64-encoded certificate data required for kubeconfig setup."
  value       = aws_eks_cluster.this.certificate_authority[0].data
  sensitive   = true
}

output "cluster_security_group_id" {
  description = "Cluster security group ID managed by EKS."
  value       = aws_eks_cluster.this.vpc_config[0].cluster_security_group_id
}

output "region" {
  description = "AWS region where the cluster is deployed."
  value       = var.aws_region
}

output "kubeconfig_command" {
  description = "AWS CLI command to merge this cluster into local kubeconfig."
  value       = "aws eks update-kubeconfig --region ${var.aws_region} --name ${aws_eks_cluster.this.name}"
}

output "cluster_admin_service_account_namespace" {
  description = "Namespace for the long-lived cluster-admin service account."
  value       = kubernetes_namespace_v1.cluster_admin.metadata[0].name
}

output "cluster_admin_service_account_name" {
  description = "Name of the long-lived cluster-admin service account."
  value       = kubernetes_service_account_v1.cluster_admin.metadata[0].name
}

output "cluster_admin_service_account_token_secret_name" {
  description = "Kubernetes Secret name that holds the long-lived service account token."
  value       = kubernetes_secret_v1.cluster_admin_token.metadata[0].name
}

output "cluster_admin_service_account_kubeconfig" {
  description = "Kubeconfig that authenticates using the long-lived service account token (not a projected token)."
  value       = local.cluster_admin_kubeconfig
  sensitive   = true
}

output "vpc_id" {
  description = "VPC ID used by the EKS cluster."
  value       = aws_vpc.this.id
}
