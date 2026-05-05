output "aks_cluster_name" {
  description = "AKS cluster name"
  value       = azurerm_kubernetes_cluster.main.name
}

output "aks_cluster_fqdn" {
  description = "AKS cluster FQDN"
  value       = azurerm_kubernetes_cluster.main.fqdn
}

output "postgres_fqdn" {
  description = "Azure PostgreSQL Flexible Server FQDN"
  value       = azurerm_postgresql_flexible_server.main.fqdn
}

output "resource_group" {
  description = "Azure resource group name"
  value       = azurerm_resource_group.main.name
}

output "cyoda_namespace" {
  description = "Kubernetes namespace where cyoda is deployed"
  value       = module.cyoda.namespace
}

output "kubeconfig_command" {
  description = "Command to update kubeconfig for this cluster"
  value       = "az aks get-credentials --resource-group ${azurerm_resource_group.main.name} --name ${azurerm_kubernetes_cluster.main.name}"
}
