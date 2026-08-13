output "controlplane_url" {
  description = "External control-plane URL. Uses the CloudFront URL when enable_cloudfront is true."
  value       = module.controlplane.controlplane_url
}

output "worker_controlplane_url" {
  description = "Worker-facing control-plane URL."
  value       = local.worker_controlplane_url
}

output "public_url" {
  description = "Customer-managed control-plane URL."
  value       = var.enable_cloudfront ? null : var.public_url
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

output "controlplane_task_subnet_ids" {
  description = "Subnet IDs used by controlplane and migration Fargate tasks."
  value       = module.controlplane.controlplane_task_subnet_ids
}

output "controlplane_security_group_id" {
  description = "Control-plane task security group ID."
  value       = module.controlplane.controlplane_security_group_id
}

output "controlplane_task_security_group_ids" {
  description = "Security group IDs attached to controlplane, dispatcher, and migration tasks."
  value       = module.controlplane.controlplane_task_security_group_ids
}

output "migration_task_definition_arn" {
  description = "ECS task definition ARN for running database migrations."
  value       = module.controlplane.migration_task_definition_arn
}

output "database_bootstrap_task_definition_arn" {
  description = "ECS task definition ARN for creating the application database role."
  value       = module.controlplane.database_bootstrap_task_definition_arn
}

output "postgres_endpoint" {
  description = "Postgres endpoint."
  value       = module.controlplane.postgres_endpoint
}

output "redis_endpoint" {
  description = "ElastiCache event stream primary endpoint."
  value       = module.controlplane.redis_endpoint
}

output "redis_url" {
  description = "Redis/Valkey URL used by controlplane."
  value       = module.controlplane.redis_url
}

output "database_master_user_secret_arn" {
  description = "RDS-managed master user secret ARN."
  value       = module.controlplane.database_master_user_secret_arn
}

output "secret_arns" {
  description = "Secrets to populate before starting services."
  value       = module.controlplane.secret_arns
}

output "worker_enrollment_secret_arn" {
  description = "Initial Worker Group enrollment token secret ARN."
  value       = module.controlplane.worker_enrollment_secret_arn
}

output "controlplane_nat_gateway_id" {
  description = "Control Plane VPC NAT Gateway ID."
  value       = module.controlplane_network.nat_gateway_id
}

output "execution_nat_gateway_id" {
  description = "Execution VPC NAT Gateway ID."
  value       = module.execution_network.nat_gateway_id
}

output "worker_autoscaling_group_name" {
  description = "Current execution Worker Auto Scaling group name."
  value       = try(module.worker_group[local.worker_pool_name].autoscaling_group_name, null)
}

output "worker_autoscaling_group_arn" {
  description = "Exact current execution Worker Auto Scaling group ARN."
  value       = try(module.worker_group[local.worker_pool_name].autoscaling_group_arn, null)
}

output "worker_protect_from_scale_in" {
  description = "Whether new execution Worker instances start protected from scale in."
  value       = try(module.worker_group[local.worker_pool_name].protect_from_scale_in, null)
}

output "worker_generation_definitions" {
  description = "Complete immutable generation inputs keyed by Product Pool name. Persist an old entry in retained_worker_generations until its restore authority is explicitly retired."
  value = {
    for pool_name, generation in local.worker_generations :
    pool_name => {
      generation_inputs = generation.generation_inputs
      min_size          = generation.min_size
      max_size          = generation.max_size
      sealed_provider_definition = try(
        module.worker_group[pool_name].sealed_provider_definition,
        generation.sealed_provider_definition,
      )
    }
  }
}

output "worker_generation_bindings" {
  description = "Exact Product Pool to provider ASG/launch-template bindings for all current and retained generations."
  value = {
    for pool_name, generation in local.worker_generations :
    pool_name => {
      provider_name           = generation.provider_name
      autoscaling_group_name  = try(module.worker_group[pool_name].autoscaling_group_name, null)
      autoscaling_group_arn   = try(module.worker_group[pool_name].autoscaling_group_arn, null)
      launch_template_id      = try(module.worker_group[pool_name].launch_template_id, null)
      launch_template_version = try(module.worker_group[pool_name].launch_template_version, null)
      ami_id                  = generation.ami_id
      min_size                = generation.min_size
      max_size                = generation.max_size
    }
  }
}
