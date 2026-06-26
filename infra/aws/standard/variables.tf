variable "aws_region" {
  description = "AWS region."
  type        = string
}

variable "name" {
  description = "Name prefix for Helmr resources. Keep this short because several AWS resources add suffixes."
  type        = string
  default     = "helmr-standard"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,22}[a-z0-9]$", var.name))
    error_message = "name must be 3-24 characters, start with a lowercase letter, end with a lowercase letter or number, and contain only lowercase letters, numbers, and hyphens."
  }
}

variable "environment" {
  description = "Environment tag value."
  type        = string
  default     = "production"
}

variable "tags" {
  description = "Additional tags applied to all resources."
  type        = map(string)
  default     = {}
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
  default     = "10.90.0.0/16"
}

variable "availability_zone_count" {
  description = "Number of Availability Zones to use. Standard is intentionally a two-AZ baseline."
  type        = number
  default     = 2

  validation {
    condition     = var.availability_zone_count == 2
    error_message = "availability_zone_count must be 2 for the standard example."
  }
}

variable "public_url" {
  description = "External URL for the control plane when enable_cloudfront is false."
  type        = string
  default     = null
  nullable    = true
}

variable "cloudfront_origin_domain_name" {
  description = "DNS name CloudFront uses for the HTTPS ALB origin when enable_cloudfront is true. This name must resolve to the public ALB and be covered by certificate_arn."
  type        = string
  default     = null
  nullable    = true
}

variable "helmr_version" {
  description = "Helmr release version to deploy, for example vX.Y.Z. Used to resolve official control and worker artifacts."
  type        = string

  validation {
    condition     = trimspace(var.helmr_version) != ""
    error_message = "helmr_version must not be empty."
  }
}

variable "release_artifacts_manifest_base_url" {
  description = "HTTPS base URL containing per-version aws-artifacts.json files."
  type        = string
  default     = "https://github.com/helmrdotdev/helmr/releases/download"
  nullable    = true
}

variable "release_artifacts_manifest_url" {
  description = "Full HTTPS URL for the release artifact manifest. Overrides release_artifacts_manifest_base_url when set."
  type        = string
  default     = null
  nullable    = true
}

variable "control_image" {
  description = "Optional digest-pinned control image URI override for custom builds. When null, the release artifact manifest is used."
  type        = string
  default     = null
  nullable    = true
}

variable "create_control_service" {
  description = "Create the ECS service after image, secrets, and migrations are ready."
  type        = bool
  default     = false
}

variable "control_desired_count" {
  description = "Desired ECS task count for the control service."
  type        = number
  default     = 2
}

variable "dispatcher_desired_count" {
  description = "Desired ECS task count for helmr-dispatcher."
  type        = number
  default     = 1
}

variable "control_health_check_path" {
  description = "HTTP path used by the control-plane target group health check."
  type        = string
  default     = "/readyz"
}

variable "email_provider" {
  description = "Email delivery provider for magic links and run wait notifications."
  type        = string
  default     = "none"
}

variable "email_from" {
  description = "Sender address for email delivery."
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

variable "certificate_arn" {
  description = "ACM certificate ARN for the control-plane HTTPS listener."
  type        = string
  default     = null
  nullable    = true
}

variable "enable_cloudfront" {
  description = "Create the module-provided CloudFront distribution in front of the ALB."
  type        = bool
  default     = false
}

variable "redis_node_type" {
  description = "ElastiCache node type for the dispatch queue."
  type        = string
  default     = "cache.t4g.small"
}

variable "redis_node_count" {
  description = "Number of ElastiCache nodes for the dispatch queue. Values greater than 1 enable automatic failover and Multi-AZ."
  type        = number
  default     = 2
}

variable "github_oauth_client_id" {
  description = "GitHub OAuth application client ID."
  type        = string
}

variable "database_instance_class" {
  description = "RDS Postgres instance class."
  type        = string
  default     = "db.t4g.small"
}

variable "database_allocated_storage_gb" {
  description = "RDS allocated storage in GiB."
  type        = number
  default     = 20
}

variable "database_multi_az" {
  description = "Create a standby RDS instance in another Availability Zone."
  type        = bool
  default     = true
}

variable "database_deletion_protection" {
  description = "Protect the RDS instance from accidental deletion."
  type        = bool
  default     = true
}

variable "database_backup_retention_days" {
  description = "RDS automated backup retention in days."
  type        = number
  default     = 14
}

variable "database_skip_final_snapshot" {
  description = "Skip the final RDS snapshot on destroy."
  type        = bool
  default     = false
}

variable "database_performance_insights_enabled" {
  description = "Enable RDS Performance Insights when supported by the chosen instance class."
  type        = bool
  default     = true
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
  description = "Secrets Manager recovery window in days."
  type        = number
  default     = 30
}

variable "worker_ami_id" {
  description = "Optional worker AMI override for custom builds. When null and create_worker is true, the release artifact manifest is used."
  type        = string
  default     = null
  nullable    = true
}

variable "create_worker" {
  description = "Create worker EC2 Auto Scaling resources."
  type        = bool
  default     = false
}

variable "worker_instance_type" {
  description = "EC2 instance type for workers."
  type        = string
  default     = "m7i.metal-24xl"
}

variable "worker_enable_nested_virtualization" {
  description = "Enable EC2 nested virtualization for supported worker instance families."
  type        = bool
  default     = false
}

variable "worker_enable_ssm" {
  description = "Enable SSM Session Manager access for worker instances."
  type        = bool
  default     = true
}

variable "worker_desired_capacity" {
  description = "Desired worker instance count."
  type        = number
  default     = 0
}

variable "worker_min_size" {
  description = "Minimum worker instance count."
  type        = number
  default     = 0
}

variable "worker_max_size" {
  description = "Maximum worker instance count."
  type        = number
  default     = 3
}

variable "worker_root_volume_size_gb" {
  description = "Worker root EBS volume size in GiB."
  type        = number
  default     = 500
}

variable "worker_root_volume_iops" {
  description = "Worker root EBS volume IOPS."
  type        = number
  default     = 12000
}

variable "worker_root_volume_throughput" {
  description = "Worker root EBS volume throughput in MiB/s."
  type        = number
  default     = 500
}

variable "worker_disk_mib" {
  description = "Optional filesystem capacity advertised by workers in MiB. Leave null to auto-detect."
  type        = number
  default     = null
  nullable    = true
}

variable "worker_vm_vcpus" {
  description = "vCPU count assigned to each worker Firecracker task VM."
  type        = number
  default     = 2
}

variable "worker_vm_memory_mib" {
  description = "Memory in MiB assigned to each worker Firecracker task VM."
  type        = number
  default     = 4096
}

variable "worker_vm_scratch_disk_mib" {
  description = "Writable disk in MiB assigned to each worker Firecracker task VM."
  type        = number
  default     = 32768
}

variable "secret_encryption_key_old_arn" {
  description = "Optional Secrets Manager ARN for HELMR_SECRET_ENCRYPTION_KEY_OLD during Helmr-managed secret re-encryption."
  type        = string
  default     = null
  nullable    = true
}

variable "secret_encryption_key_old_kms_key_arns" {
  description = "Optional customer-managed KMS key ARNs needed to decrypt secret_encryption_key_old_arn when it is not encrypted by the control module KMS key."
  type        = list(string)
  default     = []
}
