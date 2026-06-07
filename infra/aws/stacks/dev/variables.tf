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

variable "email_provider" {
  description = "Email delivery provider for magic links and waitpoint notifications."
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
