output "namespace" {
  description = "Kubernetes namespace where cyoda is deployed"
  value       = kubernetes_namespace.cyoda.metadata[0].name
}

output "release_name" {
  description = "Helm release name"
  value       = helm_release.cyoda.name
}

output "service_name" {
  description = "Kubernetes Service name for cyoda"
  value       = "${helm_release.cyoda.name}-cyoda"
}

output "hmac_secret_name" {
  description = "Name of the Kubernetes Secret holding the HMAC secret"
  value       = kubernetes_secret.hmac.metadata[0].name
}
