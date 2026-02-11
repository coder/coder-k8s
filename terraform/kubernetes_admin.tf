locals {
  eks_get_token_args = concat(
    [
      "eks",
      "get-token",
      "--region",
      var.aws_region,
      "--cluster-name",
      aws_eks_cluster.this.name,
      "--output",
      "json",
    ],
    var.aws_profile == null ? [] : ["--profile", var.aws_profile],
  )
}

provider "kubernetes" {
  host                   = aws_eks_cluster.this.endpoint
  cluster_ca_certificate = base64decode(aws_eks_cluster.this.certificate_authority[0].data)

  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = local.eks_get_token_args
  }
}

resource "kubernetes_namespace_v1" "cluster_admin" {
  metadata {
    name = var.cluster_admin_namespace
  }
}

resource "kubernetes_service_account_v1" "cluster_admin" {
  metadata {
    name      = var.cluster_admin_service_account_name
    namespace = kubernetes_namespace_v1.cluster_admin.metadata[0].name
  }
}

resource "kubernetes_secret_v1" "cluster_admin_token" {
  metadata {
    name      = "${var.cluster_admin_service_account_name}-token"
    namespace = kubernetes_namespace_v1.cluster_admin.metadata[0].name

    annotations = {
      "kubernetes.io/service-account.name" = kubernetes_service_account_v1.cluster_admin.metadata[0].name
    }
  }

  type                           = "kubernetes.io/service-account-token"
  wait_for_service_account_token = true
}

resource "kubernetes_cluster_role_binding_v1" "cluster_admin" {
  metadata {
    name = "${var.cluster_admin_service_account_name}-cluster-admin"
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "ClusterRole"
    name      = "cluster-admin"
  }

  subject {
    api_group = ""
    kind      = "ServiceAccount"
    name      = kubernetes_service_account_v1.cluster_admin.metadata[0].name
    namespace = kubernetes_service_account_v1.cluster_admin.metadata[0].namespace
  }
}

locals {
  cluster_admin_context_name = "${aws_eks_cluster.this.name}-${var.cluster_admin_service_account_name}"

  cluster_admin_kubeconfig = yamlencode({
    apiVersion = "v1"
    kind       = "Config"

    clusters = [
      {
        name = aws_eks_cluster.this.name
        cluster = {
          server                       = aws_eks_cluster.this.endpoint
          "certificate-authority-data" = aws_eks_cluster.this.certificate_authority[0].data
        }
      },
    ]

    users = [
      {
        name = var.cluster_admin_service_account_name
        user = {
          token = kubernetes_secret_v1.cluster_admin_token.data["token"]
        }
      },
    ]

    contexts = [
      {
        name = local.cluster_admin_context_name
        context = {
          cluster   = aws_eks_cluster.this.name
          user      = var.cluster_admin_service_account_name
          namespace = var.cluster_admin_namespace
        }
      },
    ]

    "current-context" = local.cluster_admin_context_name
  })
}
