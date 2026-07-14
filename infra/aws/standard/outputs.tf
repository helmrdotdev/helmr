output "control_url" {
  description = "External control-plane URL. Uses the CloudFront URL when enable_cloudfront is true."
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

output "public_url" {
  description = "Customer-managed control-plane URL."
  value       = var.enable_cloudfront ? null : var.public_url
}

output "control_cloudfront_domain_name" {
  description = "CloudFront distribution domain name when enable_cloudfront is true."
  value       = module.control.cloudfront_distribution_domain_name
}

output "control_load_balancer_dns_name" {
  description = "Control-plane load balancer DNS name."
  value       = module.control.load_balancer_dns_name
}

output "private_control_load_balancer_dns_name" {
  description = "Private worker-facing load balancer DNS name when enabled."
  value       = module.control.private_load_balancer_dns_name
}

output "control_image" {
  description = "Resolved control-plane image URI."
  value       = module.release_artifacts.control_image
}

output "worker_ami_id" {
  description = "Resolved worker AMI ID when create_worker is true or worker_ami_id is overridden."
  value       = module.release_artifacts.worker_ami_id
}

output "release_artifacts_manifest_url" {
  description = "Release artifact manifest URL used for resolution."
  value       = module.release_artifacts.manifest_url
}

output "control_cluster_name" {
  description = "ECS cluster name for helmr-control."
  value       = module.control.control_cluster_name
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

output "control_security_group_id" {
  description = "Control-plane task security group ID."
  value       = module.control.control_security_group_id
}

output "control_task_security_group_ids" {
  description = "Security group IDs attached to control, dispatcher, and migration tasks."
  value       = module.control.control_task_security_group_ids
}

output "migration_task_definition_arn" {
  description = "ECS task definition ARN for running database migrations."
  value       = module.control.migration_task_definition_arn
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

output "database_master_user_secret_arn" {
  description = "RDS-managed master user secret ARN."
  value       = module.control.database_master_user_secret_arn
}

output "secret_arns" {
  description = "Secrets to populate before starting services."
  value       = module.control.secret_arns
}

output "nat_gateway_id" {
  description = "NAT Gateway ID."
  value       = module.network.nat_gateway_id
}

output "worker_autoscaling_group_name" {
  description = "Run-worker Auto Scaling group name."
  value       = try(module.worker_group["run"].autoscaling_group_name, null)
}

output "worker_autoscaling_group_arn" {
  description = "Exact run-worker Auto Scaling group ARN."
  value       = try(module.worker_group["run"].autoscaling_group_arn, null)
}

output "worker_protect_from_scale_in" {
  description = "Whether new run-worker instances start protected from scale in."
  value       = try(module.worker_group["run"].protect_from_scale_in, null)
}

output "build_worker_autoscaling_group_name" {
  description = "Build-worker Auto Scaling group name."
  value       = try(module.worker_group["build"].autoscaling_group_name, null)
}

output "build_worker_autoscaling_group_arn" {
  description = "Exact build-worker Auto Scaling group ARN."
  value       = try(module.worker_group["build"].autoscaling_group_arn, null)
}

output "build_worker_protect_from_scale_in" {
  description = "Whether new build-worker instances start protected from scale in."
  value       = try(module.worker_group["build"].protect_from_scale_in, null)
}
