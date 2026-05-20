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

variable "setup_enabled" {
  description = "Allow browser login to create the initial owner when no owner exists. Managed deployments that bootstrap organizations elsewhere should set this to false."
  type        = bool
  default     = true
}

variable "bootstrap_owner_email" {
  description = "Email address allowed to create the initial owner when setup is enabled. Leave empty for managed deployments that bootstrap organizations elsewhere."
  type        = string
  default     = ""
}

variable "github_app_id" {
  description = "GitHub App ID."
  type        = string
}

variable "github_app_slug" {
  description = "GitHub App slug used for the public installation URL."
  type        = string
}

variable "github_app_client_id" {
  description = "GitHub App OAuth client ID."
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

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
