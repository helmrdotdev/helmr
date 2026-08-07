output "kms_key_arn" {
  description = "KMS key ARN for Helmr resources."
  value       = aws_kms_key.helmr.arn
}

output "cas_bucket_arn" {
  description = "CAS bucket ARN."
  value       = aws_s3_bucket.cas.arn
}

output "cas_bucket_name" {
  description = "CAS bucket name."
  value       = aws_s3_bucket.cas.bucket
}

output "cas_uri" {
  description = "CAS URI for CAS_URI."
  value       = "s3://${aws_s3_bucket.cas.bucket}"
}

output "postgres_endpoint" {
  description = "RDS Postgres endpoint."
  value       = aws_db_instance.postgres.endpoint
}

output "postgres_identifier" {
  description = "RDS Postgres instance identifier."
  value       = aws_db_instance.postgres.identifier
}

output "postgres_security_group_id" {
  description = "Postgres security group ID."
  value       = aws_security_group.postgres.id
}

output "redis_endpoint" {
  description = "ElastiCache dispatch primary endpoint."
  value       = aws_elasticache_replication_group.dispatch.primary_endpoint_address
}

output "redis_url" {
  description = "Redis/Valkey URL used by helmr-controlplane and helmr-dispatcher."
  value       = local.redis_url
}

output "redis_security_group_id" {
  description = "Redis/Valkey security group ID."
  value       = aws_security_group.redis.id
}

output "controlplane_url" {
  description = "Configured external control-plane URL."
  value       = local.controlplane_url
}

output "load_balancer_dns_name" {
  description = "Control-plane load balancer DNS name."
  value       = aws_lb.controlplane.dns_name
}

output "load_balancer_zone_id" {
  description = "Control-plane load balancer Route53 hosted zone ID."
  value       = aws_lb.controlplane.zone_id
}

output "cloudfront_distribution_domain_name" {
  description = "CloudFront distribution domain name when enable_cloudfront is true."
  value       = try(aws_cloudfront_distribution.controlplane[0].domain_name, null)
}

output "controlplane_security_group_id" {
  description = "Control-plane task security group ID."
  value       = aws_security_group.controlplane.id
}

output "controlplane_task_security_group_ids" {
  description = "Security group IDs attached to controlplane, dispatcher, and migration tasks."
  value       = local.controlplane_security_group_ids
}

output "controlplane_task_subnet_ids" {
  description = "Subnet IDs used by controlplane and migration Fargate tasks."
  value       = local.controlplane_subnet_ids
}

output "controlplane_assign_public_ip" {
  description = "Whether controlplane and migration Fargate tasks assign public IPs."
  value       = var.controlplane_assign_public_ip
}

output "controlplane_cluster_name" {
  description = "ECS cluster name for helmr-controlplane."
  value       = aws_ecs_cluster.controlplane.name
}

output "controlplane_cluster_arn" {
  description = "ECS cluster ARN shared with one-shot deployment tasks."
  value       = aws_ecs_cluster.controlplane.arn
}

output "controlplane_service_name" {
  description = "ECS service name for helmr-controlplane."
  value       = try(aws_ecs_service.controlplane[0].name, null)
}

output "dispatcher_service_name" {
  description = "ECS service name for helmr-dispatcher."
  value       = try(aws_ecs_service.dispatcher[0].name, null)
}

output "migration_task_definition_arn" {
  description = "ECS task definition ARN for running database migrations."
  value       = aws_ecs_task_definition.migration.arn
}

output "database_bootstrap_task_definition_arn" {
  description = "ECS task definition ARN for creating the application database role."
  value       = aws_ecs_task_definition.database_bootstrap.arn
}

output "database_master_user_secret_arn" {
  description = "RDS-managed master user secret ARN."
  value       = aws_db_instance.postgres.master_user_secret[0].secret_arn
}

output "secret_arns" {
  description = "Secrets Manager container ARNs created by the controlplane module. Populate secret values out-of-band."
  value = merge({
    database_url               = aws_secretsmanager_secret.database_url.arn
    worker_token_signing_key   = aws_secretsmanager_secret.worker_token_signing_key.arn
    auth_key                   = aws_secretsmanager_secret.auth_key.arn
    encryption_key             = aws_secretsmanager_secret.encryption_key.arn
    workspace_fencing_key      = aws_secretsmanager_secret.workspace_fencing_key.arn
    token_credential_key       = aws_secretsmanager_secret.token_credential_key.arn
    github_oauth_client_secret = aws_secretsmanager_secret.github_oauth_client_secret.arn
    checkpoint_encryption_key  = aws_secretsmanager_secret.checkpoint_encryption_key.arn
    },
    var.deployment_mode == "self-hosted" ? {
      setup_token = aws_secretsmanager_secret.setup_token[0].arn
    } : {},
    var.email_provider == "resend" ? {
      resend_api_key = aws_secretsmanager_secret.resend_api_key[0].arn
    } : {},
    var.email_provider == "smtp" && var.smtp_password_enabled ? {
      smtp_password = aws_secretsmanager_secret.smtp_password[0].arn
    } : {}
  )
}

output "worker_enrollment_secret_arn" {
  description = "Enrollment token secret shared by the initial Worker Group and its Workers."
  value       = var.bootstrap_enabled ? aws_secretsmanager_secret.worker_enrollment[0].arn : null
}

output "image_cache_registry_authority" {
  description = "Canonical ECR registry authority injected into Control Plane and Workers."
  value       = local.image_cache_registry_authority
}

output "image_cache_repository_prefix" {
  description = "Execution image-cache repository namespace."
  value       = local.image_cache_repository_prefix
}

output "image_cache_role_arn" {
  description = "Regional Execution image-cache role assumed only by build-capable Workers."
  value       = aws_iam_role.image_cache.arn
}

output "image_cache_repository_arn_prefix" {
  description = "Exact Environment repository ARN prefix used to derive Worker session policy resources."
  value       = local.image_cache_repository_arn_prefix
}
