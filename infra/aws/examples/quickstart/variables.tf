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
  description = "Create a CloudFront distribution using the default cloudfront.net domain and certificate."
  type        = bool
  default     = true
}

variable "public_url" {
  description = "External control-plane URL when enable_cloudfront is false. Ignored when enable_cloudfront is true."
  type        = string
  default     = null
  nullable    = true
}

variable "certificate_arn" {
  description = "ACM certificate ARN for a direct HTTPS ALB listener when enable_cloudfront is false."
  type        = string
  default     = null
  nullable    = true
}

variable "allow_insecure_http" {
  description = "Allow a public plaintext HTTP listener. Keep false unless this is an isolated evaluation environment."
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
  description = "Optional control image URI override for custom builds. When null, the release artifact manifest is used."
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

variable "setup_enabled" {
  description = "Allow browser login to create the initial owner."
  type        = bool
  default     = true
}

variable "bootstrap_owner_email" {
  description = "Email address allowed to create the initial owner."
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

variable "worker_ami_id" {
  description = "Optional worker AMI override for custom builds. When null and create_worker is true, the release artifact manifest is used."
  type        = string
  default     = null
  nullable    = true
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
  description = "Enable SSM Session Manager access for worker hosts."
  type        = bool
  default     = true
}

variable "worker_desired_capacity" {
  description = "Desired worker host count when create_worker is true."
  type        = number
  default     = 1
}

variable "worker_min_size" {
  description = "Minimum worker host count when create_worker is true."
  type        = number
  default     = 1
}

variable "worker_max_size" {
  description = "Maximum worker host count when create_worker is true."
  type        = number
  default     = 1
}
