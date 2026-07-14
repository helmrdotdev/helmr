variable "aws_region" {
  description = "AWS region for the quickstart deployment."
  type        = string
}

variable "name" {
  description = "Name prefix for Helmr resources. Keep this short because several AWS resources add suffixes."
  type        = string
  default     = "helmr-quickstart"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,22}[a-z0-9]$", var.name))
    error_message = "name must be 3-24 characters, start with a lowercase letter, end with a lowercase letter or number, and contain only lowercase letters, numbers, and hyphens."
  }
}

variable "environment" {
  description = "Environment tag value."
  type        = string
  default     = "quickstart"
}

variable "tags" {
  description = "Additional tags applied to all resources."
  type        = map(string)
  default     = {}
}

variable "vpc_cidr" {
  description = "CIDR block for the Helmr VPC."
  type        = string
  default     = "10.80.0.0/16"
}

variable "availability_zone_count" {
  description = "Number of availability zones for public and private subnets."
  type        = number
  default     = 2

  validation {
    condition     = var.availability_zone_count >= 2
    error_message = "availability_zone_count must be at least 2."
  }
}

variable "enable_nat_gateway" {
  description = "Create a NAT Gateway for private subnet egress. The default no-NAT mode uses public IPs for control and migration Fargate tasks."
  type        = bool
  default     = false
}

variable "bucket_name_prefix" {
  description = "Globally unique S3 bucket name prefix. Defaults to name-account-region inside the control module."
  type        = string
  default     = null
  nullable    = true
}

variable "enable_cloudfront" {
  description = "Create a CloudFront distribution using the default cloudfront.net viewer domain in front of an HTTPS ALB origin."
  type        = bool
  default     = false
}

variable "public_url" {
  description = "External control-plane URL when enable_cloudfront is false. Ignored when enable_cloudfront is true."
  type        = string
  default     = null
  nullable    = true
}

variable "deployment_mode" {
  description = "Helmr deployment mode passed to control-plane tasks."
  type        = string
  default     = "self-hosted"
}

variable "worker_group_id" {
  description = "Default run-worker group ID for this stack."
  type        = string

  validation {
    condition     = trimspace(var.worker_group_id) != ""
    error_message = "worker_group_id must be non-empty."
  }
}

variable "region_id" {
  description = "Helmr region primitive for this stack. Defaults to aws_region."
  type        = string
  default     = null
  nullable    = true
}

variable "default_region_id" {
  description = "Default execution region for newly created projects and environments. Defaults to region_id."
  type        = string
  default     = null
  nullable    = true
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
}

variable "clickhouse_password_kms_key_arns" {
  description = "Optional customer-managed KMS key ARNs needed to decrypt clickhouse_password_secret_arn."
  type        = list(string)
  default     = []
}

variable "additional_control_security_group_ids" {
  description = "Additional security groups attached to control, dispatcher, and migration tasks."
  type        = list(string)
  default     = []
}

variable "cloudfront_origin_domain_name" {
  description = "DNS name CloudFront uses for the HTTPS ALB origin when enable_cloudfront is true. This name must resolve to the public ALB and be covered by certificate_arn."
  type        = string
  default     = null
  nullable    = true
}

variable "certificate_arn" {
  description = "ACM certificate ARN for the HTTPS ALB listener."
  type        = string
  default     = null
  nullable    = true
}

variable "allow_insecure_http" {
  description = "Allow a public plaintext HTTP forwarding listener. Keep false unless this is an isolated evaluation environment."
  type        = bool
  default     = false
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
  description = "Create the ECS service. Keep false for the first apply until image and secret values are ready."
  type        = bool
  default     = false
}

variable "control_desired_count" {
  description = "Desired ECS task count for the control service."
  type        = number
  default     = 1
}

variable "dispatcher_desired_count" {
  description = "Desired ECS task count for helmr-dispatcher."
  type        = number
  default     = 1
}

variable "control_assign_public_ip" {
  description = "Run control and migration Fargate tasks in public subnets with public IPs so the quickstart can avoid NAT Gateway."
  type        = bool
  default     = true
}

variable "control_health_check_path" {
  description = "HTTP path used by the control-plane target group health check."
  type        = string
  default     = "/healthz"

  validation {
    condition     = startswith(var.control_health_check_path, "/")
    error_message = "control_health_check_path must start with /."
  }
}

variable "control_environment" {
  description = "Additional non-secret environment variables for helmr-control."
  type        = map(string)
  default     = {}
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

variable "redis_node_type" {
  description = "ElastiCache node type for the dispatch queue."
  type        = string
  default     = "cache.t4g.micro"
}

variable "redis_node_count" {
  description = "Number of ElastiCache nodes for the dispatch queue."
  type        = number
  default     = 1
}

variable "github_oauth_client_id" {
  description = "GitHub OAuth application client ID."
  type        = string
}

variable "database_instance_class" {
  description = "RDS Postgres instance class."
  type        = string
  default     = "db.t4g.micro"
}

variable "database_engine_version" {
  description = "RDS Postgres engine version. Set null to use the AWS default for the region."
  type        = string
  default     = null
  nullable    = true
}

variable "database_allocated_storage_gb" {
  description = "RDS allocated storage in GiB."
  type        = number
  default     = 20
}

variable "database_backup_retention_days" {
  description = "RDS automated backup retention in days."
  type        = number
  default     = 1
}

variable "database_performance_insights_enabled" {
  description = "Enable RDS Performance Insights when supported by the chosen instance class."
  type        = bool
  default     = false
}

variable "database_deletion_protection" {
  description = "Protect the RDS instance from accidental deletion."
  type        = bool
  default     = false
}

variable "database_skip_final_snapshot" {
  description = "Skip the final RDS snapshot on destroy."
  type        = bool
  default     = true
}

variable "control_log_retention_days" {
  description = "CloudWatch Logs retention in days for control and migration tasks."
  type        = number
  default     = 7
}

variable "kms_deletion_window_in_days" {
  description = "KMS key deletion window in days."
  type        = number
  default     = 7
}

variable "secret_recovery_window_in_days" {
  description = "Secrets Manager recovery window in days."
  type        = number
  default     = 7
}

variable "cas_object_expiration_days" {
  description = "Days before current CAS objects expire."
  type        = number
  default     = 7
  nullable    = true
}

variable "cas_noncurrent_version_expiration_days" {
  description = "Days before noncurrent CAS object versions expire."
  type        = number
  default     = 1
  nullable    = true
}

variable "create_worker" {
  description = "Create worker EC2 Auto Scaling resources. Defaults off; enable for a single nested-virtualization smoke worker."
  type        = bool
  default     = false
}

variable "worker_fleet_controller" {
  description = "Run/build fleet-controller policy used whenever worker groups are created."
  type = object({
    run_warm_workers             = optional(number, 0)
    build_warm_workers           = optional(number, 0)
    run_max_workers              = optional(number)
    build_max_workers            = optional(number)
    max_scale_out_per_cycle      = optional(number, 1)
    max_pending_workers          = optional(number, 1)
    max_packing_items            = optional(number, 10000)
    controller_interval_seconds  = optional(number, 15)
    scale_out_cooldown_seconds   = optional(number, 30)
    scale_in_cooldown_seconds    = optional(number, 300)
    scale_in_hysteresis_seconds  = optional(number, 300)
    stale_worker_timeout_seconds = optional(number, 120)
    readiness_timeout_seconds    = optional(number, 900)
    drain_timeout_seconds        = optional(number, 1800)
    emergency_stop               = optional(bool, false)
    metric_interval_seconds      = optional(number, 60)
  })
  default = {}
}

variable "worker_ami_id" {
  description = "Optional worker AMI override for custom builds. When null and create_worker is true, the release artifact manifest is used."
  type        = string
  default     = null
  nullable    = true
}

variable "worker_allowed_ami_ids" {
  description = "Additional worker AMIs accepted during a rolling worker replacement. Remove superseded AMIs after the refresh completes."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for ami_id in var.worker_allowed_ami_ids : can(regex("^ami-[0-9a-fA-F]+$", ami_id))])
    error_message = "worker_allowed_ami_ids must contain AWS AMI IDs."
  }
}

variable "worker_instance_type" {
  description = "EC2 instance type for the smoke worker."
  type        = string
  default     = "c8i.xlarge"
}

variable "worker_enable_nested_virtualization" {
  description = "Enable EC2 nested virtualization for supported worker instance families."
  type        = bool
  default     = true
}

variable "worker_enable_ssm" {
  description = "Enable SSM Session Manager access for worker instances."
  type        = bool
  default     = true
}

variable "worker_min_size" {
  description = "Minimum worker instance count when create_worker is true."
  type        = number
  default     = 1
}

variable "worker_max_size" {
  description = "Maximum worker instance count when create_worker is true."
  type        = number
  default     = 1
}

variable "build_worker_min_size" {
  description = "Minimum build-worker instance count."
  type        = number
  default     = 0
}

variable "build_worker_max_size" {
  description = "Maximum build-worker instance count."
  type        = number
  default     = 3
}

variable "build_worker_instance_type" {
  type     = string
  default  = null
  nullable = true
}
variable "worker_capacity_vcpus" {
  type     = number
  default  = null
  nullable = true
}
variable "worker_capacity_memory_mib" {
  type     = number
  default  = null
  nullable = true
}
variable "worker_execution_slots" {
  type     = number
  default  = null
  nullable = true
}
variable "worker_substrate_cache_max_mib" {
  type     = number
  default  = null
  nullable = true
}
variable "worker_artifact_cache_max_mib" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_enable_nested_virtualization" {
  type     = bool
  default  = null
  nullable = true
}
variable "build_worker_root_volume_size_gb" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_root_volume_iops" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_root_volume_throughput" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_disk_mib" {
  type     = number
  default  = null
  nullable = true
}

variable "build_worker_disk_reserve_mib" {
  description = "Build-worker filesystem reserve in MiB. Defaults to worker_disk_reserve_mib."
  type        = number
  default     = null
  nullable    = true

  validation {
    condition     = var.build_worker_disk_reserve_mib == null || var.build_worker_disk_reserve_mib > 0
    error_message = "build_worker_disk_reserve_mib must be null or positive."
  }
}
variable "build_worker_vm_vcpus" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_vm_memory_mib" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_vm_scratch_disk_mib" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_capacity_vcpus" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_capacity_memory_mib" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_execution_slots" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_substrate_cache_max_mib" {
  type     = number
  default  = null
  nullable = true
}
variable "build_worker_artifact_cache_max_mib" {
  type     = number
  default  = null
  nullable = true
}

variable "worker_root_volume_size_gb" {
  description = "Smoke worker root EBS volume size in GiB."
  type        = number
  default     = 120
}

variable "worker_root_volume_iops" {
  description = "Smoke worker root EBS volume IOPS."
  type        = number
  default     = 3000
}

variable "worker_root_volume_throughput" {
  description = "Smoke worker root EBS volume throughput in MiB/s."
  type        = number
  default     = 125
}

variable "worker_disk_mib" {
  description = "Optional filesystem capacity ceiling in MiB before the worker reserve is withheld. Leave null to auto-detect."
  type        = number
  default     = null
  nullable    = true
}

variable "worker_disk_reserve_mib" {
  description = "Filesystem capacity in MiB withheld from advertised worker capacity."
  type        = number
  default     = 1024

  validation {
    condition     = var.worker_disk_reserve_mib > 0
    error_message = "worker_disk_reserve_mib must be positive."
  }
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
