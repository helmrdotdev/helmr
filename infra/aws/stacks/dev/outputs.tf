output "cas_uri" {
  description = "CAS URI for worker configuration."
  value       = module.control.cas_uri
}

output "control_url" {
  description = "Configured external control-plane URL."
  value       = module.control.control_url
}

output "worker_control_url" {
  description = "Worker-facing control-plane URL."
  value       = local.worker_control_url
}

output "private_control_dns_name" {
  description = "Private Route53 control DNS name when enabled."
  value       = local.private_control_dns_name
}

output "control_load_balancer_dns_name" {
  description = "Control-plane load balancer DNS name."
  value       = module.control.load_balancer_dns_name
}

output "private_control_load_balancer_dns_name" {
  description = "Private worker-facing load balancer DNS name when enabled."
  value       = module.control.private_load_balancer_dns_name
}

output "control_cloudfront_domain_name" {
  description = "CloudFront distribution domain name when enabled."
  value       = module.control.cloudfront_distribution_domain_name
}

output "control_ecr_repository_url" {
  description = "ECR repository URL for the control-plane image."
  value       = module.control.control_ecr_repository_url
}

output "control_cluster_name" {
  description = "ECS cluster name for helmr-control."
  value       = module.control.control_cluster_name
}

output "control_security_group_id" {
  description = "Control-plane task security group ID."
  value       = module.control.control_security_group_id
}

output "control_service_name" {
  description = "ECS service name for helmr-control."
  value       = module.control.control_service_name
}

output "dispatcher_service_name" {
  description = "ECS service name for helmr-dispatcher."
  value       = module.control.dispatcher_service_name
}

output "control_task_subnet_ids" {
  description = "Subnet IDs used by control and migration Fargate tasks."
  value       = module.control.control_task_subnet_ids
}

output "control_assign_public_ip" {
  description = "Whether control and migration Fargate tasks assign public IPs."
  value       = module.control.control_assign_public_ip
}

output "nat_gateway_id" {
  description = "NAT Gateway ID when enabled."
  value       = module.network.nat_gateway_id
}

output "migration_task_definition_arn" {
  description = "ECS task definition ARN for running database migrations."
  value       = module.control.migration_task_definition_arn
}

output "private_subnet_ids" {
  description = "Private subnet IDs for ECS run-task network configuration."
  value       = module.network.private_subnet_ids
}

output "postgres_endpoint" {
  description = "Postgres endpoint."
  value       = module.control.postgres_endpoint
}

output "redis_endpoint" {
  description = "ElastiCache dispatch primary endpoint."
  value       = module.control.redis_endpoint
}

output "redis_url" {
  description = "Redis/Valkey URL used by control and dispatcher."
  value       = module.control.redis_url
}

output "postgres_identifier" {
  description = "RDS Postgres instance identifier."
  value       = module.control.postgres_identifier
}

output "database_master_user_secret_arn" {
  description = "RDS-managed master user secret ARN."
  value       = module.control.database_master_user_secret_arn
}

output "secret_arns" {
  description = "Secrets to populate before starting services."
  value       = module.control.secret_arns
}

output "worker_autoscaling_group_name" {
  description = "Worker Auto Scaling group name."
  value       = try(module.worker[0].autoscaling_group_name, null)
}

output "worker_iam_role_name" {
  description = "Worker IAM role name."
  value       = try(module.worker[0].iam_role_name, null)
}
