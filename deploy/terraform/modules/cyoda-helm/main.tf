terraform {
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.31"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.14"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

locals {
  postgres_secret_name  = "cyoda-postgres"
  jwt_secret_name       = "cyoda-jwt"
  hmac_secret_name      = "cyoda-hmac"
  bootstrap_secret_name = "cyoda-bootstrap"

  hmac_secret_value = var.hmac_secret != "" ? var.hmac_secret : random_password.hmac[0].result
}

resource "random_password" "hmac" {
  count   = var.hmac_secret == "" ? 1 : 0
  length  = 32
  special = false
}

resource "kubernetes_namespace" "cyoda" {
  metadata {
    name = var.namespace
    labels = {
      "app.kubernetes.io/managed-by" = "terraform"
    }
  }
}

resource "kubernetes_secret" "postgres" {
  metadata {
    name      = local.postgres_secret_name
    namespace = kubernetes_namespace.cyoda.metadata[0].name
  }
  data = {
    dsn = var.postgres_dsn
  }
  type = "Opaque"
}

resource "kubernetes_secret" "jwt" {
  metadata {
    name      = local.jwt_secret_name
    namespace = kubernetes_namespace.cyoda.metadata[0].name
  }
  data = {
    "signing-key.pem" = var.jwt_signing_key_pem
  }
  type = "Opaque"
}

resource "kubernetes_secret" "hmac" {
  metadata {
    name      = local.hmac_secret_name
    namespace = kubernetes_namespace.cyoda.metadata[0].name
  }
  data = {
    secret = local.hmac_secret_value
  }
  type = "Opaque"
}

resource "kubernetes_secret" "bootstrap" {
  count = var.bootstrap_client_id != "" ? 1 : 0
  metadata {
    name      = local.bootstrap_secret_name
    namespace = kubernetes_namespace.cyoda.metadata[0].name
  }
  data = {
    secret = var.bootstrap_client_secret
  }
  type = "Opaque"
}

resource "helm_release" "cyoda" {
  name       = "cyoda"
  chart      = var.chart_path
  namespace  = kubernetes_namespace.cyoda.metadata[0].name
  wait       = true
  timeout    = 600

  set {
    name  = "replicas"
    value = var.replicas
  }

  set {
    name  = "logLevel"
    value = var.log_level
  }

  set {
    name  = "image.tag"
    value = var.image_tag
  }

  set {
    name  = "service.type"
    value = var.service_type
  }

  set {
    name  = "gateway.enabled"
    value = "false"
  }

  set {
    name  = "postgres.existingSecret"
    value = local.postgres_secret_name
  }

  set {
    name  = "jwt.existingSecret"
    value = local.jwt_secret_name
  }

  set {
    name  = "jwt.issuer"
    value = var.jwt_issuer
  }

  set {
    name  = "jwt.expirySeconds"
    value = var.jwt_expiry_seconds
  }

  set {
    name  = "cluster.hmacSecret.existingSecret"
    value = local.hmac_secret_name
  }

  set {
    name  = "bootstrap.clientId"
    value = var.bootstrap_client_id
  }

  dynamic "set" {
    for_each = var.bootstrap_client_id != "" ? [1] : []
    content {
      name  = "bootstrap.clientSecret.existingSecret"
      value = local.bootstrap_secret_name
    }
  }

  set {
    name  = "resources.requests.cpu"
    value = var.resources.requests.cpu
  }

  set {
    name  = "resources.requests.memory"
    value = var.resources.requests.memory
  }

  set {
    name  = "resources.limits.cpu"
    value = var.resources.limits.cpu
  }

  set {
    name  = "resources.limits.memory"
    value = var.resources.limits.memory
  }

  depends_on = [
    kubernetes_secret.postgres,
    kubernetes_secret.jwt,
    kubernetes_secret.hmac,
  ]
}
