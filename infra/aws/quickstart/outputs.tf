output "controlplane_url" {
  description = "External control-plane URL. With the default CloudFront mode this is the generated cloudfront.net URL."
  value       = module.controlplane.controlplane_url
}

output "worker_controlplane_url" {
  description = "Worker-facing control-plane URL."
  value       = local.worker_controlplane_url
}

output "controlplane_cloudfront_domain_name" {
  description = "CloudFront distribution domain name when enable_cloudfront is true."
  value       = module.controlplane.cloudfront_distribution_domain_name
}

output "controlplane_load_balancer_dns_name" {
  description = "Control-plane load balancer DNS name."
  value       = module.controlplane.load_balancer_dns_name
}

output "controlplane_image" {
  description = "Resolved control-plane image URI."
  value       = module.release_artifacts.controlplane_image
}

output "worker_ami_id" {
  description = "Resolved worker AMI ID when create_worker is true or worker_ami_id is overridden."
  value       = module.release_artifacts.worker_ami_id
}

output "release_artifacts_manifest_url" {
  description = "Release artifact manifest URL used for resolution."
  value       = module.release_artifacts.manifest_url
}

output "controlplane_cluster_name" {
  description = "ECS cluster name for helmr-controlplane."
  value       = module.controlplane.controlplane_cluster_name
}

output "controlplane_service_name" {
  description = "ECS service name for helmr-controlplane."
  value       = module.controlplane.controlplane_service_name
}

output "dispatcher_service_name" {
  description = "ECS service name for helmr-dispatcher."
  value       = module.controlplane.dispatcher_service_name
}

output "controlplane_security_group_id" {
  description = "Control-plane task security group ID."
  value       = module.controlplane.controlplane_security_group_id
}

output "controlplane_task_security_group_ids" {
  description = "Security group IDs attached to controlplane, dispatcher, and migration tasks."
  value       = module.controlplane.controlplane_task_security_group_ids
}

output "controlplane_task_subnet_ids" {
  description = "Subnet IDs used by controlplane and migration Fargate tasks."
  value       = module.controlplane.controlplane_task_subnet_ids
}

output "controlplane_assign_public_ip" {
  description = "Whether controlplane and migration Fargate tasks assign public IPs."
  value       = module.controlplane.controlplane_assign_public_ip
}

output "migration_task_definition_arn" {
  description = "ECS task definition ARN for running database migrations."
  value       = module.controlplane.migration_task_definition_arn
}

output "cas_uri" {
  description = "CAS URI for worker configuration."
  value       = module.controlplane.cas_uri
}

output "cas_bucket_name" {
  description = "CAS bucket name."
  value       = module.controlplane.cas_bucket_name
}

output "postgres_endpoint" {
  description = "RDS Postgres endpoint."
  value       = module.controlplane.postgres_endpoint
}

output "redis_endpoint" {
  description = "ElastiCache dispatch primary endpoint."
  value       = module.controlplane.redis_endpoint
}

output "redis_url" {
  description = "Redis/Valkey URL used by controlplane and dispatcher."
  value       = module.controlplane.redis_url
}

output "postgres_identifier" {
  description = "RDS Postgres instance identifier."
  value       = module.controlplane.postgres_identifier
}

output "database_master_user_secret_arn" {
  description = "RDS-managed master user secret ARN."
  value       = module.controlplane.database_master_user_secret_arn
}

output "secret_arns" {
  description = "Secrets Manager ARNs to populate outside Terraform before starting services."
  value       = module.controlplane.secret_arns
}

output "worker_enrollment_secret_arns" {
  description = "Per-worker-group enrollment secret ARNs."
  value       = module.controlplane.worker_enrollment_secret_arns
}

output "controlplane_nat_gateway_id" {
  description = "Control Plane VPC NAT Gateway ID when enabled."
  value       = module.controlplane_network.nat_gateway_id
}

output "execution_nat_gateway_id" {
  description = "Execution VPC NAT Gateway ID when enabled."
  value       = module.execution_network.nat_gateway_id
}

output "worker_autoscaling_group_name" {
  description = "Run-worker Auto Scaling group name when create_worker is true."
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

output "worker_iam_role_name" {
  description = "Run-worker IAM role name when create_worker is true."
  value       = try(module.worker_group["run"].iam_role_name, null)
}

output "build_worker_autoscaling_group_name" {
  description = "Build-worker Auto Scaling group name when create_worker is true."
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

output "build_worker_iam_role_name" {
  description = "Build-worker IAM role name when create_worker is true."
  value       = try(module.worker_group["build"].iam_role_name, null)
}
