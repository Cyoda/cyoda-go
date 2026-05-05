variable "namespace" {
  description = "Kubernetes namespace to deploy cyoda into"
  type        = string
  default     = "cyoda"
}

variable "chart_path" {
  description = "Path to the cyoda Helm chart (relative or absolute)"
  type        = string
  default     = "../../helm/cyoda"
}

variable "image_tag" {
  description = "cyoda container image tag. Defaults to chart appVersion when empty."
  type        = string
  default     = ""
}

variable "replicas" {
  description = "Number of cyoda pods"
  type        = number
  default     = 1
}

variable "log_level" {
  description = "Log level: debug, info, warn, error"
  type        = string
  default     = "info"
}

variable "service_type" {
  description = "Kubernetes Service type: ClusterIP, LoadBalancer, NodePort"
  type        = string
  default     = "LoadBalancer"
}

variable "postgres_dsn" {
  description = "Full PostgreSQL connection string, e.g. postgres://user:pass@host:5432/db?sslmode=require"
  type        = string
  sensitive   = true
}

variable "jwt_signing_key_pem" {
  description = "PEM-encoded RSA private key for JWT signing"
  type        = string
  sensitive   = true
}

variable "jwt_issuer" {
  description = "JWT issuer claim (iss)"
  type        = string
  default     = "cyoda"
}

variable "jwt_expiry_seconds" {
  description = "JWT token lifetime in seconds"
  type        = number
  default     = 3600
}

variable "hmac_secret" {
  description = "Hex-encoded HMAC secret for inter-node dispatch auth. Auto-generated when empty."
  type        = string
  sensitive   = true
  default     = ""
}

variable "bootstrap_client_id" {
  description = "Bootstrap M2M client ID. Leave empty to disable."
  type        = string
  default     = ""
}

variable "bootstrap_client_secret" {
  description = "Bootstrap M2M client secret. Required when bootstrap_client_id is set."
  type        = string
  sensitive   = true
  default     = ""
}

variable "extra_env" {
  description = "Additional environment variables. Each entry: {name, value} or {name, valueFrom}."
  type        = list(map(string))
  default     = []
}

variable "resources" {
  description = "CPU/memory requests and limits"
  type = object({
    requests = object({ cpu = string, memory = string })
    limits   = object({ cpu = string, memory = string })
  })
  default = {
    requests = { cpu = "100m", memory = "256Mi" }
    limits   = { cpu = "1000m", memory = "512Mi" }
  }
}
