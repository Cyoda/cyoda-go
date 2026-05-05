output "gke_cluster_name" {
  description = "GKE cluster name"
  value       = google_container_cluster.main.name
}

output "gke_cluster_endpoint" {
  description = "GKE cluster API endpoint"
  value       = "https://${google_container_cluster.main.endpoint}"
}

output "cloud_sql_private_ip" {
  description = "Cloud SQL private IP address"
  value       = google_sql_database_instance.postgres.private_ip_address
}

output "cloud_sql_instance_name" {
  description = "Cloud SQL instance name"
  value       = google_sql_database_instance.postgres.name
}

output "cyoda_namespace" {
  description = "Kubernetes namespace where cyoda is deployed"
  value       = module.cyoda.namespace
}

output "kubeconfig_command" {
  description = "Command to update kubeconfig for this cluster"
  value       = "gcloud container clusters get-credentials ${google_container_cluster.main.name} --region ${var.region} --project ${var.project_id}"
}
