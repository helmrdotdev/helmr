variable "name" {
  description = "Name prefix for control-plane resources."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,17}[a-z0-9]$", var.name))
    error_message = "name must be 3-19 characters so names with the -controlplane suffix fit AWS limits; it must start with a lowercase letter, end with a lowercase letter or number, and contain only lowercase letters, numbers, and hyphens."
  }
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
  description = "External HTTPS URL for the direct ALB Control Plane when enable_cloudfront is false."
  type        = string
  default     = null
  nullable    = true
}

variable "api_origin" {
  description = "External HTTPS origin used for machine-facing Control Plane API URLs. Defaults to the effective public URL."
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

variable "bootstrap_enabled" {
  description = "Create the initial Region and Worker Group when they do not exist."
  type        = bool
  default     = true
}

variable "bootstrap_worker_group_name" {
  description = "Immutable Region-scoped name of the initial Worker Group."
  type        = string
  default     = "default"

  validation {
    condition     = can(regex("^[a-z0-9](?:[a-z0-9-]{0,126}[a-z0-9])?$", var.bootstrap_worker_group_name))
    error_message = "bootstrap_worker_group_name must be a lowercase URL-safe identifier of 1-128 characters."
  }
}

variable "bootstrap_region_id" {
  description = "ID of the initial Region. Defaults to the AWS provider region."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition = var.bootstrap_region_id == null ? true : (
      var.bootstrap_region_id != "" &&
      var.bootstrap_region_id == trimspace(var.bootstrap_region_id) &&
      length(base64encode(var.bootstrap_region_id)) <= 340 &&
      length(regexall("[[:cntrl:]]", var.bootstrap_region_id)) == 0
    )
    error_message = "bootstrap_region_id must be null or normalized control-free UTF-8 of 1-255 bytes."
  }
}

variable "bootstrap_region_display_name" {
  description = "Display name of the initial Region. Defaults to bootstrap_region_id."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.bootstrap_region_display_name == null || trimspace(var.bootstrap_region_display_name) != ""
    error_message = "bootstrap_region_display_name must be null or non-empty."
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
  description = "Secrets Manager ARN for CLICKHOUSE_PASSWORD when the ClickHouse endpoint requires a password."
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

variable "controlplane_image" {
  description = "Container image URI containing helmr-controlplane, helmr-dispatcher, and deployment tooling. Managed release flows should pass a digest-pinned image."
  type        = string

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.controlplane_image))
    error_message = "controlplane_image must be pinned by a lowercase sha256 digest."
  }
}

variable "controlplane_image_repository_arn" {
  description = "Exact ECR repository ARN from which ECS task execution roles may pull the Control Plane image. Leave null only for a non-ECR self-hosted image."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition = (
      var.controlplane_image_repository_arn == null ||
      can(regex("^arn:[^:]+:ecr:[a-z0-9-]+:[0-9]{12}:repository/[a-z0-9._/-]+$", var.controlplane_image_repository_arn))
    )
    error_message = "controlplane_image_repository_arn must be null or an ECR repository ARN."
  }
}

variable "platform_store_uri" {
  description = "Dedicated immutable Platform Artifact store URI ending in /objects."
  type        = string

  validation {
    condition     = can(regex("^s3://[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]/objects$", var.platform_store_uri))
    error_message = "platform_store_uri must be an S3 bucket URI ending exactly in /objects."
  }
}

variable "platform_store_bucket_arn" {
  description = "S3 bucket ARN backing platform_store_uri."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:s3:::[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.platform_store_bucket_arn))
    error_message = "platform_store_bucket_arn must be an S3 bucket ARN."
  }
}

variable "platform_store_kms_key_arn" {
  description = "KMS key ARN used by the dedicated Platform Artifact store."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:kms:[^:]+:[0-9]{12}:key/[0-9a-fA-F-]+$", var.platform_store_kms_key_arn))
    error_message = "platform_store_kms_key_arn must be a KMS key ARN."
  }
}

variable "controlplane_entrypoint" {
  description = "Container entrypoint for helmr-controlplane."
  type        = list(string)
  default     = ["helmr-controlplane"]
}

variable "controlplane_cpu" {
  description = "Fargate CPU units for helmr-controlplane."
  type        = number
  default     = 512
}

variable "controlplane_memory" {
  description = "Fargate memory in MiB for helmr-controlplane."
  type        = number
  default     = 1024
}

variable "controlplane_architecture" {
  description = "Fargate CPU architecture for helmr-controlplane."
  type        = string
  default     = "X86_64"

  validation {
    condition     = contains(["X86_64", "ARM64"], var.controlplane_architecture)
    error_message = "controlplane_architecture must be X86_64 or ARM64."
  }
}

variable "controlplane_desired_count" {
  description = "Desired helmr-controlplane task count."
  type        = number
  default     = 2
}

variable "dispatcher_desired_count" {
  description = "Desired helmr-dispatcher task count."
  type        = number
  default     = 1
}

variable "controlplane_assign_public_ip" {
  description = "Assign public IPs and run controlplane/migration Fargate tasks in public subnets. Useful for dev stacks without NAT Gateway."
  type        = bool
  default     = false
}

variable "create_controlplane_service" {
  description = "Create the ECS service. Keep false until image, secrets, and migrations are ready."
  type        = bool
  default     = false
}

variable "capacity_token_secret_arn" {
  description = "Optional externally owned Secrets Manager ARN containing the provider-neutral capacity credential."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.capacity_token_secret_arn == null || can(regex("^arn:[^:]+:secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]+$", var.capacity_token_secret_arn))
    error_message = "capacity_token_secret_arn must be a Secrets Manager secret ARN."
  }
}

variable "capacity_token_kms_key_arn" {
  description = "Optional KMS key ARN required to decrypt capacity_token_secret_arn."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.capacity_token_kms_key_arn == null || can(regex("^arn:[^:]+:kms:[a-z0-9-]+:[0-9]{12}:key/[0-9a-f-]+$", var.capacity_token_kms_key_arn))
    error_message = "capacity_token_kms_key_arn must be a KMS key ARN."
  }
}

variable "controlplane_environment" {
  description = "Additional non-secret environment variables for helmr-controlplane. Managed Helmr variables such as REDIS_URL are owned by this module."
  type        = map(string)
  default     = {}
}

variable "dispatcher_environment" {
  description = "Additional non-secret environment variables for helmr-dispatcher. Managed Helmr variables are owned by this module."
  type        = map(string)
  default     = {}
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
  description = "ElastiCache engine for the Helmr Control Plane event stream."
  type        = string
  default     = "valkey"

  validation {
    condition     = contains(["valkey", "redis"], var.redis_engine)
    error_message = "redis_engine must be valkey or redis."
  }
}

variable "redis_node_type" {
  description = "ElastiCache node type for the Helmr Control Plane event stream."
  type        = string
  default     = "cache.t4g.micro"
}

variable "redis_node_count" {
  description = "Number of ElastiCache nodes for the Helmr Control Plane event stream. Values greater than 1 enable automatic failover and Multi-AZ."
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

variable "database_instance_class" {
  description = "RDS Postgres instance class."
  type        = string
  default     = "db.t4g.micro"
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

variable "controlplane_log_retention_days" {
  description = "CloudWatch Logs retention in days for controlplane and migration tasks."
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

variable "additional_controlplane_security_group_ids" {
  description = "Additional security groups attached to controlplane, dispatcher, and migration tasks."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for id in var.additional_controlplane_security_group_ids : trimspace(id) != ""])
    error_message = "additional_controlplane_security_group_ids entries must be non-empty security group IDs."
  }
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
