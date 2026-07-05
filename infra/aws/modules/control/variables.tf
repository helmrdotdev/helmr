variable "name" {
  description = "Name prefix for control-plane resources."
  type        = string
}

variable "bucket_name_prefix" {
  description = "Globally unique prefix for S3 buckets. Defaults to name-account-region."
  type        = string
  default     = null
  nullable    = true
}

variable "vpc_id" {
  description = "VPC ID."
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnet IDs for control-plane tasks and Postgres."
  type        = list(string)
}

variable "public_subnet_ids" {
  description = "Public subnet IDs for the control-plane load balancer."
  type        = list(string)
}

variable "public_url" {
  description = "External HTTPS URL for the direct ALB control plane when enable_cloudfront is false."
  type        = string
  default     = null
  nullable    = true
}

variable "deployment_mode" {
  description = "Helmr deployment mode passed to control-plane tasks."
  type        = string
  default     = "self-hosted"

  validation {
    condition     = contains(["self-hosted", "managed-cloud"], var.deployment_mode)
    error_message = "deployment_mode must be self-hosted or managed-cloud."
  }
}

variable "cell_id" {
  description = "Opaque cell ID for this control-plane stack."
  type        = string

  validation {
    condition     = trimspace(var.cell_id) != ""
    error_message = "cell_id must be non-empty."
  }
}

variable "region_id" {
  description = "Helmr region primitive for this control-plane stack. Defaults to the AWS provider region."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.region_id == null || trimspace(var.region_id) != ""
    error_message = "region_id must be null or non-empty."
  }
}

variable "default_region_id" {
  description = "Default execution region for newly created projects and environments. Defaults to region_id."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.default_region_id == null || trimspace(var.default_region_id) != ""
    error_message = "default_region_id must be null or non-empty."
  }
}

variable "region_display_name" {
  description = "Display name stored for the Helmr region. Defaults to region_id."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.region_display_name == null || trimspace(var.region_display_name) != ""
    error_message = "region_display_name must be null or non-empty."
  }
}

variable "cell_environment_class" {
  description = "Environment class stored for the bootstrapped cell. Defaults to deployment_mode."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.cell_environment_class == null || trimspace(var.cell_environment_class) != ""
    error_message = "cell_environment_class must be null or non-empty."
  }
}

variable "clickhouse_url" {
  description = "ClickHouse HTTP endpoint for historical telemetry."
  type        = string

  validation {
    condition     = can(regex("^https://[^<>[:space:]]+(:[0-9]+)?/?$", trimspace(var.clickhouse_url)))
    error_message = "clickhouse_url must be an https URL without placeholder characters."
  }
}

variable "clickhouse_user" {
  description = "Optional ClickHouse username for historical telemetry."
  type        = string
  default     = null
  nullable    = true
}

variable "clickhouse_password_secret_arn" {
  description = "Secrets Manager ARN for HELMR_CLICKHOUSE_PASSWORD when the ClickHouse endpoint requires a password."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.clickhouse_password_secret_arn == null || can(regex("^arn:aws[a-zA-Z-]*:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", trimspace(var.clickhouse_password_secret_arn)))
    error_message = "clickhouse_password_secret_arn must be null or a Secrets Manager secret ARN."
  }
}

variable "clickhouse_password_kms_key_arns" {
  description = "Optional customer-managed KMS key ARNs needed to decrypt clickhouse_password_secret_arn when it is not encrypted by this module's KMS key."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for arn in var.clickhouse_password_kms_key_arns : trimspace(arn) != ""])
    error_message = "clickhouse_password_kms_key_arns entries must be non-empty KMS key ARNs."
  }
}

variable "control_image" {
  description = "Container image URI containing helmr-control and helmr-dispatcher. Managed release flows should pass a digest-pinned image."
  type        = string
}

variable "control_entrypoint" {
  description = "Container entrypoint for helmr-control."
  type        = list(string)
  default     = ["helmr-control"]
}

variable "control_cpu" {
  description = "Fargate CPU units for helmr-control."
  type        = number
  default     = 512
}

variable "control_memory" {
  description = "Fargate memory in MiB for helmr-control."
  type        = number
  default     = 1024
}

variable "control_architecture" {
  description = "Fargate CPU architecture for helmr-control."
  type        = string
  default     = "X86_64"

  validation {
    condition     = contains(["X86_64", "ARM64"], var.control_architecture)
    error_message = "control_architecture must be X86_64 or ARM64."
  }
}

variable "control_desired_count" {
  description = "Desired helmr-control task count."
  type        = number
  default     = 2
}

variable "dispatcher_desired_count" {
  description = "Desired helmr-dispatcher task count."
  type        = number
  default     = 1
}

variable "schedule_repair_every" {
  description = "Schedule repair polling interval."
  type        = string
  default     = "5s"

  validation {
    condition     = can(regex("^[1-9]", var.schedule_repair_every))
    error_message = "schedule_repair_every must be a positive duration."
  }
}

variable "schedule_repair_limit" {
  description = "Maximum schedule repair entries scanned or due fires claimed per worker tick."
  type        = number
  default     = 100

  validation {
    condition     = var.schedule_repair_limit > 0
    error_message = "schedule_repair_limit must be positive."
  }
}

variable "schedule_trigger_concurrency" {
  description = "Maximum concurrent scheduled run creation operations per dispatcher task."
  type        = number
  default     = 10

  validation {
    condition     = var.schedule_trigger_concurrency > 0
    error_message = "schedule_trigger_concurrency must be positive."
  }
}

variable "schedule_repair_lookahead" {
  description = "How far ahead schedule workers repair database next-fire state into Redis."
  type        = string
  default     = "1h"

  validation {
    condition     = can(regex("^[1-9]", var.schedule_repair_lookahead))
    error_message = "schedule_repair_lookahead must be a positive duration."
  }
}

variable "schedule_lease" {
  description = "Redis schedule fire visibility lease duration."
  type        = string
  default     = "5m"

  validation {
    condition     = can(regex("^[1-9]", var.schedule_lease))
    error_message = "schedule_lease must be a positive duration."
  }
}

variable "schedule_max_attempts" {
  description = "Maximum attempts for one schedule fire before it is skipped with an error."
  type        = number
  default     = 10

  validation {
    condition     = var.schedule_max_attempts > 0
    error_message = "schedule_max_attempts must be positive."
  }
}

variable "schedule_jitter" {
  description = "Stable distribution window applied to schedule fire eligibility."
  type        = string
  default     = "30s"

  validation {
    condition     = can(regex("^[1-9]", var.schedule_jitter))
    error_message = "schedule_jitter must be a positive duration."
  }
}

variable "control_assign_public_ip" {
  description = "Assign public IPs and run control/migration Fargate tasks in public subnets. Useful for dev stacks without NAT Gateway."
  type        = bool
  default     = false
}

variable "control_health_check_path" {
  description = "HTTP path used by the control-plane target group health check. /readyz gates traffic on database schema readiness; /healthz is useful for staged rollouts from older images."
  type        = string
  default     = "/healthz"

  validation {
    condition     = startswith(var.control_health_check_path, "/")
    error_message = "control_health_check_path must start with /."
  }
}

variable "create_control_service" {
  description = "Create the ECS service. Keep false until image, secrets, and migrations are ready."
  type        = bool
  default     = false
}

variable "control_environment" {
  description = "Additional non-secret environment variables for helmr-control. Managed Helmr variables such as HELMR_REDIS_URL are owned by this module."
  type        = map(string)
  default     = {}
}

variable "dispatcher_environment" {
  description = "Additional non-secret environment variables for helmr-dispatcher. Managed Helmr variables such as HELMR_REDIS_URL are owned by this module."
  type        = map(string)
  default     = {}
}

variable "secret_encryption_key_old_arn" {
  description = "Optional Secrets Manager ARN for HELMR_SECRET_ENCRYPTION_KEY_OLD during Helmr-managed secret re-encryption."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.secret_encryption_key_old_arn == null || trimspace(var.secret_encryption_key_old_arn) != ""
    error_message = "secret_encryption_key_old_arn must be null or a non-empty Secrets Manager ARN."
  }
}

variable "secret_encryption_key_old_kms_key_arns" {
  description = "Optional customer-managed KMS key ARNs needed to decrypt secret_encryption_key_old_arn when it is not encrypted by this module's KMS key."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for arn in var.secret_encryption_key_old_kms_key_arns : trimspace(arn) != ""])
    error_message = "secret_encryption_key_old_kms_key_arns entries must be non-empty KMS key ARNs."
  }
}

variable "email_provider" {
  description = "Email delivery provider for magic links and run wait notifications."
  type        = string
  default     = "none"

  validation {
    condition     = contains(["none", "log", "smtp", "resend"], var.email_provider)
    error_message = "email_provider must be one of none, log, smtp, or resend."
  }
}

variable "email_from" {
  description = "Sender address for email delivery, such as Helmr <noreply@example.com>."
  type        = string
  default     = null
  nullable    = true
}

variable "smtp_addr" {
  description = "SMTP host:port when email_provider is smtp."
  type        = string
  default     = null
  nullable    = true
}

variable "smtp_username" {
  description = "SMTP username when email_provider is smtp."
  type        = string
  default     = null
  nullable    = true
}

variable "smtp_password_enabled" {
  description = "Create and inject an SMTP password secret when email_provider is smtp."
  type        = bool
  default     = false
}

variable "redis_engine" {
  description = "ElastiCache engine for the Helmr dispatch queue."
  type        = string
  default     = "valkey"

  validation {
    condition     = contains(["valkey", "redis"], var.redis_engine)
    error_message = "redis_engine must be valkey or redis."
  }
}

variable "redis_node_type" {
  description = "ElastiCache node type for the Helmr dispatch queue."
  type        = string
  default     = "cache.t4g.micro"
}

variable "redis_node_count" {
  description = "Number of ElastiCache nodes for the Helmr dispatch queue. Values greater than 1 enable automatic failover and Multi-AZ."
  type        = number
  default     = 1
}

variable "github_oauth_client_id" {
  description = "GitHub OAuth application client ID."
  type        = string
}

variable "certificate_arn" {
  description = "ACM certificate ARN for HTTPS."
  type        = string
  default     = null
  nullable    = true
}

variable "allow_insecure_http" {
  description = "Allow an internet-facing plaintext HTTP forwarding listener. Intended for development only; when certificate_arn is set, false redirects HTTP to HTTPS."
  type        = bool
  default     = false
}

variable "enable_cloudfront" {
  description = "Create a CloudFront distribution with the default cloudfront.net viewer certificate in front of an HTTPS control-plane ALB origin."
  type        = bool
  default     = false
}

variable "cloudfront_origin_domain_name" {
  description = "DNS name CloudFront uses for the HTTPS ALB origin. This must resolve to the public ALB and be covered by certificate_arn."
  type        = string
  default     = null
  nullable    = true
}

variable "private_control_dns_name" {
  description = "Optional VPC-private DNS name for worker-to-control traffic. Use a hostname covered by certificate_arn."
  type        = string
  default     = null
  nullable    = true
}

variable "database_instance_class" {
  description = "RDS Postgres instance class."
  type        = string
  default     = "db.t4g.micro"
}

variable "database_engine_version" {
  description = "RDS Postgres engine version. Set to null to use the AWS default for the region."
  type        = string
  default     = "18.2"
  nullable    = true
}

variable "database_allocated_storage_gb" {
  description = "RDS allocated storage in GiB."
  type        = number
  default     = 20
}

variable "database_multi_az" {
  description = "Create a standby RDS instance in another Availability Zone."
  type        = bool
  default     = false
}

variable "database_backup_retention_days" {
  description = "RDS automated backup retention in days."
  type        = number
  default     = 7
}

variable "database_performance_insights_enabled" {
  description = "Enable RDS Performance Insights when supported by the chosen instance class."
  type        = bool
  default     = false
}

variable "database_deletion_protection" {
  description = "Protect the RDS instance from accidental deletion."
  type        = bool
  default     = true
}

variable "database_skip_final_snapshot" {
  description = "Skip the final RDS snapshot on destroy. Intended for ephemeral development stacks."
  type        = bool
  default     = false
}

variable "create_control_repository" {
  description = "Create an ECR repository for custom control images. Official release deployments can leave this false."
  type        = bool
  default     = false
}

variable "control_repository_force_delete" {
  description = "Delete the control ECR repository even when it contains images. Intended for ephemeral development stacks."
  type        = bool
  default     = false
}

variable "control_ecr_max_images" {
  description = "Maximum tagged control images to retain in ECR. Set null to disable this lifecycle rule."
  type        = number
  default     = null
  nullable    = true
}

variable "control_ecr_untagged_image_expiration_days" {
  description = "Days before untagged control images expire in ECR. Set null to disable this lifecycle rule."
  type        = number
  default     = null
  nullable    = true
}

variable "control_log_retention_days" {
  description = "CloudWatch Logs retention in days for control and migration tasks."
  type        = number
  default     = 30
}

variable "kms_deletion_window_in_days" {
  description = "KMS key deletion window in days."
  type        = number
  default     = 30
}

variable "secret_recovery_window_in_days" {
  description = "Secrets Manager recovery window in days. Use 7 for ephemeral dev stacks."
  type        = number
  default     = 30
}

variable "cas_object_expiration_days" {
  description = "Days before current CAS objects expire. Set null to disable current object expiration."
  type        = number
  default     = null
  nullable    = true
}

variable "cas_noncurrent_version_expiration_days" {
  description = "Days before noncurrent CAS object versions expire. Set null to disable noncurrent version expiration."
  type        = number
  default     = null
  nullable    = true
}

variable "allowed_security_group_ids" {
  description = "Security groups allowed to connect to Postgres."
  type        = list(string)
  default     = []
}

variable "additional_control_security_group_ids" {
  description = "Additional security groups attached to control, dispatcher, and migration tasks."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for id in var.additional_control_security_group_ids : trimspace(id) != ""])
    error_message = "additional_control_security_group_ids entries must be non-empty security group IDs."
  }
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
