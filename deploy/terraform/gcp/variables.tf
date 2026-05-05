variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "region" {
  description = "GCP region"
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = "GCP zone (used for zonal resources)"
  type        = string
  default     = "us-central1-a"
}

variable "cluster_name" {
  description = "Name for the GKE cluster and related resources"
  type        = string
  default     = "cyoda"
}

# GKE
variable "kubernetes_version" {
  description = "Minimum Kubernetes master version (e.g. '1.31'). Use 'latest' for the latest stable."
  type        = string
  default     = "latest"
}

variable "node_machine_type" {
  description = "GCE machine type for GKE nodes"
  type        = string
  default     = "e2-standard-2"
}

variable "node_count" {
  description = "Initial number of nodes per zone"
  type        = number
  default     = 2
}

variable "node_min_count" {
  description = "Minimum node count for autoscaling"
  type        = number
  default     = 1
}

variable "node_max_count" {
  description = "Maximum node count for autoscaling"
  type        = number
  default     = 4
}

# Cloud SQL
variable "db_name" {
  description = "Name of the PostgreSQL database"
  type        = string
  default     = "cyoda"
}

variable "db_username" {
  description = "PostgreSQL user"
  type        = string
  default     = "cyoda"
}

variable "db_password" {
  description = "PostgreSQL user password"
  type        = string
  sensitive   = true
}

variable "db_tier" {
  description = "Cloud SQL machine tier (e.g. db-f1-micro, db-g1-small, db-custom-2-7680)"
  type        = string
  default     = "db-g1-small"
}

variable "db_disk_size" {
  description = "Cloud SQL disk size in GiB"
  type        = number
  default     = 20
}

variable "db_availability_type" {
  description = "ZONAL or REGIONAL (multi-zone HA) for Cloud SQL"
  type        = string
  default     = "ZONAL"
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
