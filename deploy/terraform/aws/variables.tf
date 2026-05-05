variable "region" {
  description = "AWS region"
  type        = string
  default     = "us-east-1"
}

variable "cluster_name" {
  description = "Name of the EKS cluster and related resources"
  type        = string
  default     = "cyoda"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC"
  type        = string
  default     = "10.0.0.0/16"
}

variable "availability_zones" {
  description = "List of availability zones. Must have at least two for RDS subnet group."
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b"]
}

# EKS
variable "kubernetes_version" {
  description = "Kubernetes version for EKS"
  type        = string
  default     = "1.31"
}

variable "node_instance_type" {
  description = "EC2 instance type for EKS worker nodes"
  type        = string
  default     = "t3.medium"
}

variable "node_desired_size" {
  description = "Desired number of EKS worker nodes"
  type        = number
  default     = 2
}

variable "node_min_size" {
  description = "Minimum number of EKS worker nodes"
  type        = number
  default     = 1
}

variable "node_max_size" {
  description = "Maximum number of EKS worker nodes"
  type        = number
  default     = 4
}

# RDS PostgreSQL
variable "db_name" {
  description = "Name of the PostgreSQL database"
  type        = string
  default     = "cyoda"
}

variable "db_username" {
  description = "PostgreSQL admin username"
  type        = string
  default     = "cyoda"
}

variable "db_password" {
  description = "PostgreSQL admin password"
  type        = string
  sensitive   = true
}

variable "db_instance_class" {
  description = "RDS instance class"
  type        = string
  default     = "db.t3.micro"
}

variable "db_allocated_storage" {
  description = "Allocated storage in GiB for RDS"
  type        = number
  default     = 20
}

variable "db_multi_az" {
  description = "Enable Multi-AZ for RDS (recommended for production)"
  type        = bool
  default     = false
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
  description = "PEM-encoded RSA private key for JWT signing. Generate with: openssl genrsa -out key.pem 4096"
  type        = string
  sensitive   = true
}

variable "jwt_issuer" {
  description = "JWT issuer claim"
  type        = string
  default     = "cyoda"
}
