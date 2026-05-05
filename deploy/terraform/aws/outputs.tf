output "eks_cluster_name" {
  description = "EKS cluster name"
  value       = aws_eks_cluster.main.name
}

output "eks_cluster_endpoint" {
  description = "EKS API server endpoint"
  value       = aws_eks_cluster.main.endpoint
}

output "rds_endpoint" {
  description = "RDS PostgreSQL host:port"
  value       = "${aws_db_instance.postgres.address}:${aws_db_instance.postgres.port}"
}

output "rds_db_name" {
  description = "RDS database name"
  value       = aws_db_instance.postgres.db_name
}

output "cyoda_namespace" {
  description = "Kubernetes namespace where cyoda is deployed"
  value       = module.cyoda.namespace
}

output "kubeconfig_command" {
  description = "Command to update kubeconfig for this cluster"
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${aws_eks_cluster.main.name}"
}
