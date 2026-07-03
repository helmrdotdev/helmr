variable "aws_region" {
  description = "AWS region."
  type        = string
}

variable "name" {
  description = "Deployment name."
  type        = string
}

variable "public_url" {
  description = "External HTTPS URL for the direct ALB control plane when enable_cloudfront is false."
  type        = string
}

variable "deployment_mode" {
  description = "Helmr deployment mode passed to control-plane tasks."
  type        = string
  default     = "managed-cloud"
}

variable "cell_id" {
  description = "Managed-cloud cell ID for this stack."
  type        = string
  default     = "us-east-1-cell-1"
}

variable "clickhouse_url" {
  description = "External ClickHouse HTTP endpoint for historical telemetry. Leave null when create_clickhouse_cloud is true."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.clickhouse_url == null || can(regex("^https://[^<>[:space:]]+(:[0-9]+)?/?$", trimspace(var.clickhouse_url)))
    error_message = "clickhouse_url must be an https URL without placeholder characters."
  }
}

variable "create_clickhouse_cloud" {
  description = "Create a ClickHouse Cloud service, AWS PrivateLink endpoint, private DNS, and Secrets Manager password secret for dev telemetry."
  type        = bool
  default     = false
}

variable "clickhouse_organization_id" {
  description = "ClickHouse Cloud organization ID used by the ClickHouse Terraform provider when create_clickhouse_cloud is true."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.clickhouse_organization_id == null || trimspace(var.clickhouse_organization_id) != ""
    error_message = "clickhouse_organization_id must be null or a non-empty organization ID."
  }
}

variable "clickhouse_cloud_service_name" {
  description = "Optional ClickHouse Cloud service name. Defaults to <name>-telemetry."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.clickhouse_cloud_service_name == null || trimspace(var.clickhouse_cloud_service_name) != ""
    error_message = "clickhouse_cloud_service_name must be null or non-empty."
  }
}

variable "clickhouse_cloud_region" {
  description = "ClickHouse Cloud AWS region. Defaults to aws_region and must match the AWS provider region for PrivateLink."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.clickhouse_cloud_region == null || trimspace(var.clickhouse_cloud_region) != ""
    error_message = "clickhouse_cloud_region must be null or non-empty."
  }
}

variable "clickhouse_secret_kms_key_id" {
  description = "Optional customer-managed KMS key ID or ARN for the Terraform-managed ClickHouse password secret."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.clickhouse_secret_kms_key_id == null || trimspace(var.clickhouse_secret_kms_key_id) != ""
    error_message = "clickhouse_secret_kms_key_id must be null or a non-empty KMS key ID or ARN."
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

variable "clickhouse_min_replica_memory_gb" {
  description = "Minimum memory per Terraform-managed ClickHouse Cloud replica."
  type        = number
  default     = 8
}

variable "clickhouse_max_replica_memory_gb" {
  description = "Maximum memory per Terraform-managed ClickHouse Cloud replica."
  type        = number
  default     = 32
}

variable "clickhouse_idle_scaling" {
  description = "Enable idle scaling for Terraform-managed ClickHouse Cloud."
  type        = bool
  default     = true
}

variable "clickhouse_idle_timeout_minutes" {
  description = "Idle timeout in minutes for Terraform-managed ClickHouse Cloud when idle scaling is enabled."
  type        = number
  default     = 5
}

variable "clickhouse_backup_retention_period_in_hours" {
  description = "Backup retention in hours for Terraform-managed ClickHouse Cloud."
  type        = number
  default     = 24
}

variable "cloudfront_origin_domain_name" {
  description = "DNS name CloudFront uses for the HTTPS ALB origin when enable_cloudfront is true. This name must resolve to the public ALB and be covered by certificate_arn."
  type        = string
  default     = null
  nullable    = true
}

variable "enable_nat_gateway" {
  description = "Create a NAT Gateway for private subnet egress. Keep false for low-cost control-only dev."
  type        = bool
  default     = false
}

variable "control_image" {
  description = "Container image URI for helmr-control."
  type        = string
}

variable "create_control_service" {
  description = "Create the ECS service. Keep false until image, secrets, and migrations are ready."
  type        = bool
  default     = false
}

variable "control_desired_count" {
  description = "Desired ECS task count for the control service."
  type        = number
  default     = 1
}

variable "control_environment" {
  description = "Additional non-secret environment variables for helmr-control."
  type        = map(string)
  default     = {}
}

variable "dispatcher_desired_count" {
  description = "Desired ECS task count for helmr-dispatcher."
  type        = number
  default     = 1
}

variable "control_assign_public_ip" {
  description = "Run control and migration tasks in public subnets with public IPs so dev can avoid NAT Gateway."
  type        = bool
  default     = true
}

variable "control_health_check_path" {
  description = "HTTP path used by the control-plane target group health check. Use /readyz after the deployed image serves readiness checks."
  type        = string
  default     = "/healthz"
}

variable "dispatcher_environment" {
  description = "Additional non-secret environment variables for helmr-dispatcher."
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
  description = "Create a CloudFront distribution with the default cloudfront.net certificate in front of the control-plane ALB."
  type        = bool
  default     = false
}

variable "github_oauth_client_id" {
  description = "GitHub OAuth application client ID."
  type        = string
}

variable "database_deletion_protection" {
  description = "Protect the RDS instance from accidental deletion."
  type        = bool
  default     = false
}

variable "database_backup_retention_days" {
  description = "RDS automated backup retention in days."
  type        = number
  default     = 1
}

variable "database_engine_version" {
  description = "RDS Postgres engine version. Set to null to use the AWS default for the region."
  type        = string
  default     = "18.2"
  nullable    = true
}

variable "database_skip_final_snapshot" {
  description = "Skip the final RDS snapshot on destroy."
  type        = bool
  default     = true
}

variable "control_repository_force_delete" {
  description = "Delete the control ECR repository even when it contains images."
  type        = bool
  default     = true
}

variable "control_ecr_max_images" {
  description = "Maximum tagged control images to retain in ECR."
  type        = number
  default     = 10
}

variable "control_ecr_untagged_image_expiration_days" {
  description = "Days before untagged control images expire in ECR."
  type        = number
  default     = 1
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
  description = "Secrets Manager recovery window in days. Dev defaults to immediate deletion so destroy/recreate cycles can reuse stable secret names."
  type        = number
  default     = 0
}

variable "cas_object_expiration_days" {
  description = "Days before current CAS objects expire."
  type        = number
  default     = 7
}

variable "cas_noncurrent_version_expiration_days" {
  description = "Days before noncurrent CAS object versions expire."
  type        = number
  default     = 1
}

variable "worker_ami_id" {
  description = "Worker AMI with Firecracker, jailer, BuildKit, CNI plugins, and helmr-worker installed."
  type        = string
  default     = null
  nullable    = true
}

variable "create_worker" {
  description = "Create worker EC2 Auto Scaling resources. Keep false until the worker AMI is available."
  type        = bool
  default     = false
}

variable "worker_instance_type" {
  description = "EC2 instance type for workers. Prefer metal for production, or C8i/M8i/R8i with nested virtualization for smoke."
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

variable "worker_buildkit_slirp_cidr" {
  description = "IPv4 CIDR used by rootlesskit/slirp4netns inside the worker BuildKit service namespace."
  type        = string
  default     = "198.18.0.0/24"
}

variable "worker_desired_capacity" {
  description = "Desired worker instance count. Keep 0 until the worker AMI and required secrets are ready."
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
  default     = 120
}

variable "worker_root_volume_iops" {
  description = "Worker root EBS volume IOPS."
  type        = number
  default     = 3000
}

variable "worker_root_volume_throughput" {
  description = "Worker root EBS volume throughput in MiB/s."
  type        = number
  default     = 125
}

variable "worker_disk_mib" {
  description = "Optional filesystem capacity advertised by dev workers in MiB. Leave null to auto-detect."
  type        = number
  default     = null
  nullable    = true
}

variable "worker_vm_vcpus" {
  description = "vCPU count assigned to each dev Firecracker task VM."
  type        = number
  default     = 2
}

variable "worker_vm_memory_mib" {
  description = "Memory in MiB assigned to each dev Firecracker task VM."
  type        = number
  default     = 4096
}

variable "worker_vm_scratch_disk_mib" {
  description = "Writable disk in MiB assigned to each dev Firecracker task VM."
  type        = number
  default     = 32768
}

variable "worker_capacity_vcpus" {
  description = "Total vCPU capacity advertised by dev workers. Leave null to advertise one VM's vCPU count."
  type        = number
  default     = null
  nullable    = true
}

variable "worker_capacity_memory_mib" {
  description = "Total memory capacity in MiB advertised by dev workers. Leave null to advertise one VM's memory."
  type        = number
  default     = null
  nullable    = true
}

variable "worker_execution_slots" {
  description = "Total execution slots advertised by dev workers. Leave null to advertise one slot."
  type        = number
  default     = null
  nullable    = true
}

variable "worker_environment" {
  description = "Additional non-secret environment variables written to the dev worker env file."
  type        = map(string)
  default     = {}
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
