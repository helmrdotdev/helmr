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
  description = "CAS URI for HELMR_CAS_URI."
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
  description = "Redis/Valkey URL used by helmr-control and helmr-dispatcher."
  value       = local.redis_url
}

output "redis_security_group_id" {
  description = "Redis/Valkey security group ID."
  value       = aws_security_group.redis.id
}

output "async_queue_uri" {
  description = "Async queue URI for async control-plane messages."
  value       = "sqs+${aws_sqs_queue.async.url}"
}

output "async_dead_letter_queue_uri" {
  description = "Async dead-letter queue URI for async control-plane messages."
  value       = "sqs+${aws_sqs_queue.async_dlq.url}"
}

output "control_url" {
  description = "Configured external control-plane URL."
  value       = local.control_url
}

output "private_control_url" {
  description = "Private worker-facing control-plane URL when private_control_dns_name is set."
  value       = local.private_control_url
}

output "load_balancer_dns_name" {
  description = "Control-plane load balancer DNS name."
  value       = aws_lb.control.dns_name
}

output "load_balancer_zone_id" {
  description = "Control-plane load balancer Route53 hosted zone ID."
  value       = aws_lb.control.zone_id
}

output "private_load_balancer_dns_name" {
  description = "Private worker-facing load balancer DNS name when private_control_dns_name is set."
  value       = try(aws_lb.private_control[0].dns_name, null)
}

output "cloudfront_distribution_domain_name" {
  description = "CloudFront distribution domain name when enable_cloudfront is true."
  value       = try(aws_cloudfront_distribution.control[0].domain_name, null)
}

output "control_security_group_id" {
  description = "Control-plane task security group ID."
  value       = aws_security_group.control.id
}

output "control_task_subnet_ids" {
  description = "Subnet IDs used by control and migration Fargate tasks."
  value       = local.control_subnet_ids
}

output "control_assign_public_ip" {
  description = "Whether control and migration Fargate tasks assign public IPs."
  value       = var.control_assign_public_ip
}

output "control_ecr_repository_url" {
  description = "ECR repository URL for custom control-plane images when create_control_repository is true."
  value       = try(aws_ecr_repository.control[0].repository_url, null)
}

output "control_cluster_name" {
  description = "ECS cluster name for helmr-control."
  value       = aws_ecs_cluster.control.name
}

output "control_service_name" {
  description = "ECS service name for helmr-control."
  value       = try(aws_ecs_service.control[0].name, null)
}

output "dispatcher_service_name" {
  description = "ECS service name for helmr-dispatcher."
  value       = try(aws_ecs_service.dispatcher[0].name, null)
}

output "migration_task_definition_arn" {
  description = "ECS task definition ARN for running database migrations."
  value       = aws_ecs_task_definition.migration.arn
}

output "database_master_user_secret_arn" {
  description = "RDS-managed master user secret ARN."
  value       = aws_db_instance.postgres.master_user_secret[0].secret_arn
}

output "secret_arns" {
  description = "Secrets Manager container ARNs created by the control module. Populate secret values out-of-band."
  value = merge({
    database_url              = aws_secretsmanager_secret.database_url.arn
    worker_token_signing_key  = aws_secretsmanager_secret.worker_token_signing_key.arn
    worker_bootstrap_token    = aws_secretsmanager_secret.worker_bootstrap_token.arn
    setup_token               = aws_secretsmanager_secret.setup_token.arn
    auth_secret               = aws_secretsmanager_secret.auth_secret.arn
    secret_encryption_key     = aws_secretsmanager_secret.secret_encryption_key.arn
    github_app_private_key    = aws_secretsmanager_secret.github_app_private_key.arn
    github_app_webhook_secret = aws_secretsmanager_secret.github_app_webhook_secret.arn
    github_app_client_secret  = aws_secretsmanager_secret.github_app_client_secret.arn
    checkpoint_encryption_key = aws_secretsmanager_secret.checkpoint_encryption_key.arn
    },
    var.email_provider == "resend" ? {
      resend_api_key = aws_secretsmanager_secret.resend_api_key[0].arn
    } : {},
    var.email_provider == "smtp" && var.smtp_password_enabled ? {
      smtp_password = aws_secretsmanager_secret.smtp_password[0].arn
    } : {}
  )
}
