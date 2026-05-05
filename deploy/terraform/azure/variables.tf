variable "subscription_id" {
  description = "Azure subscription ID"
  type        = string
}

variable "location" {
  description = "Azure region"
  type        = string
  default     = "eastus"
}

variable "resource_group_name" {
  description = "Name of the Azure resource group (will be created)"
  type        = string
  default     = "cyoda-rg"
}

variable "cluster_name" {
  description = "Name prefix for the AKS cluster and related resources"
  type        = string
  default     = "cyoda"
}

# AKS
variable "kubernetes_version" {
  description = "Kubernetes version for AKS. Use null for the latest stable."
  type        = string
  default     = null
}

variable "node_vm_size" {
  description = "VM size for AKS nodes"
  type        = string
  default     = "Standard_D2s_v3"
}

variable "node_count" {
  description = "Initial node count"
  type        = number
  default     = 2
}

variable "node_min_count" {
  description = "Minimum node count for cluster autoscaler"
  type        = number
  default     = 1
}

variable "node_max_count" {
  description = "Maximum node count for cluster autoscaler"
  type        = number
  default     = 4
}

# Azure Database for PostgreSQL Flexible Server
variable "db_admin_username" {
  description = "PostgreSQL administrator username"
  type        = string
  default     = "cyodaadmin"
}

variable "db_admin_password" {
  description = "PostgreSQL administrator password (min 8 chars, must include uppercase, lowercase, digit, special)"
  type        = string
  sensitive   = true
}

variable "db_name" {
  description = "Name of the PostgreSQL database"
  type        = string
  default     = "cyoda"
}

variable "db_sku_name" {
  description = "SKU for Azure PostgreSQL Flexible Server (e.g. B_Standard_B1ms, GP_Standard_D2s_v3)"
  type        = string
  default     = "B_Standard_B1ms"
}

variable "db_storage_mb" {
  description = "Storage in MB for PostgreSQL Flexible Server (32768, 65536, …)"
  type        = number
  default     = 32768
}

variable "db_version" {
  description = "PostgreSQL major version"
  type        = string
  default     = "16"
}

variable "db_zone" {
  description = "Availability zone for PostgreSQL Flexible Server"
  type        = string
  default     = "1"
}

# cyoda
variable "cyoda_image_tag" {
  description = "cyoda container image tag"
  type        = string
  default     = ""
}

variable "cyoda_replicas" {
  description = "Number of cyoda pods"
  type        = number
  default     = 1
}

variable "cyoda_namespace" {
  description = "Kubernetes namespace for cyoda"
  type        = string
  default     = "cyoda"
}

variable "jwt_signing_key_pem" {
  description = "PEM-encoded RSA private key for JWT signing"
  type        = string
  sensitive   = true
}

variable "jwt_issuer" {
  description = "JWT issuer claim"
  type        = string
  default     = "cyoda"
}
