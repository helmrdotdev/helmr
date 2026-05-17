variable "aws_region" {
  description = "AWS region."
  type        = string
}

variable "name" {
  description = "Deployment name."
  type        = string
}

variable "public_url" {
  description = "External URL for the control plane. Ignored when enable_cloudfront is true."
  type        = string
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

variable "certificate_arn" {
  description = "ACM certificate ARN for HTTPS."
  type        = string
  default     = null
  nullable    = true
}

variable "allow_insecure_http" {
  description = "Allow an internet-facing plaintext HTTP listener. Intended for development only."
  type        = bool
  default     = false
}

variable "enable_cloudfront" {
  description = "Create a CloudFront distribution with the default cloudfront.net certificate in front of the control-plane ALB."
  type        = bool
  default     = false
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
  description = "Allow browser login to create the initial owner in the dev control plane."
  type        = bool
  default     = true
}

variable "bootstrap_owner_email" {
  description = "Email address allowed to create the initial owner in the dev control plane."
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
  description = "Enable SSM Session Manager access for worker hosts."
  type        = bool
  default     = true
}

variable "worker_buildkit_slirp_cidr" {
  description = "IPv4 CIDR used by rootlesskit/slirp4netns inside the worker BuildKit service namespace."
  type        = string
  default     = "198.18.0.0/24"
}

variable "worker_desired_capacity" {
  description = "Desired worker host count. Keep 0 until the worker AMI and required secrets are ready."
  type        = number
  default     = 0
}

variable "worker_min_size" {
  description = "Minimum worker host count."
  type        = number
  default     = 0
}

variable "worker_max_size" {
  description = "Maximum worker host count."
  type        = number
  default     = 3
}

variable "control_url" {
  description = "Public or private control-plane URL for workers."
  type        = string
  default     = null
  nullable    = true
}
