variable "name" {
  description = "Name prefix for worker resources."
  type        = string
}

variable "vpc_id" {
  description = "VPC ID."
  type        = string
}

variable "subnet_ids" {
  description = "Private subnet IDs for workers."
  type        = list(string)
}

variable "ami_id" {
  description = "Worker AMI with Firecracker, jailer, BuildKit, CNI plugins, and helmr-worker installed."
  type        = string
}

variable "instance_type" {
  description = "EC2 instance type for workers. Use nested virtualization on supported C8i/M8i/R8i instances for smoke, or metal for production isolation."
  type        = string
  default     = "m7i.metal-24xl"
}

variable "enable_nested_virtualization" {
  description = "Enable EC2 nested virtualization in the launch template. Supported by C8i, M8i, and R8i instances."
  type        = bool
  default     = false
}

variable "enable_ssm" {
  description = "Attach AmazonSSMManagedInstanceCore so workers can be reached through Session Manager without inbound SSH."
  type        = bool
  default     = true
}

variable "desired_capacity" {
  description = "Desired worker host count."
  type        = number
  default     = 0
}

variable "min_size" {
  description = "Minimum worker host count."
  type        = number
  default     = 0
}

variable "max_size" {
  description = "Maximum worker host count."
  type        = number
  default     = 3
}

variable "health_check_grace_period_seconds" {
  description = "ASG health check grace period for worker hosts."
  type        = number
  default     = 900
}

variable "enable_lifecycle_hooks" {
  description = "Enable launch readiness and termination drain lifecycle hooks."
  type        = bool
  default     = true
}

variable "launch_lifecycle_heartbeat_timeout_seconds" {
  description = "Seconds to wait for worker host bootstrap before the launch lifecycle hook times out."
  type        = number
  default     = 900
}

variable "termination_lifecycle_heartbeat_timeout_seconds" {
  description = "Seconds to wait for worker drain before the termination lifecycle hook times out."
  type        = number
  default     = 3600
}

variable "termination_drain_timeout_seconds" {
  description = "Maximum seconds helmr-worker drain should wait for active executions."
  type        = number
  default     = 1800
}

variable "lifecycle_heartbeat_interval_seconds" {
  description = "Seconds between lifecycle action heartbeats while draining."
  type        = number
  default     = 60
}

variable "worker_binary_path" {
  description = "Path or command name for the helmr-worker binary on the worker AMI."
  type        = string
  default     = "helmr-worker"
}

variable "root_volume_size_gb" {
  description = "Worker root EBS volume size in GiB."
  type        = number
  default     = 200
}

variable "root_volume_device_name" {
  description = "Root block device name for the worker AMI."
  type        = string
  default     = "/dev/sda1"
}

variable "root_volume_type" {
  description = "Worker root EBS volume type."
  type        = string
  default     = "gp3"

  validation {
    condition     = var.root_volume_type == "gp3"
    error_message = "root_volume_type currently supports gp3 only."
  }
}

variable "root_volume_iops" {
  description = "Worker root EBS volume IOPS. Only used by volume types that support explicit IOPS."
  type        = number
  default     = 6000
}

variable "root_volume_throughput" {
  description = "Worker root EBS volume throughput in MiB/s. Only used by volume types that support explicit throughput."
  type        = number
  default     = 250
}

variable "control_url" {
  description = "Public or private control-plane URL for HELMR_CONTROL_URL."
  type        = string
}

variable "cas_uri" {
  description = "CAS URI for HELMR_CAS_URI."
  type        = string
}

variable "cas_bucket_arn" {
  description = "S3 bucket ARN for CAS access."
  type        = string
}

variable "kms_key_arn" {
  description = "KMS key ARN for encrypted Helmr storage."
  type        = string
}

variable "secret_arns" {
  description = "Secret ARNs required by the worker."
  type = object({
    worker_pool_registration_token = string
    checkpoint_encryption_key      = string
  })
}

variable "jailer_uid" {
  description = "UID used by the Firecracker jailer."
  type        = number
  default     = 1001
}

variable "jailer_gid" {
  description = "GID used by the Firecracker jailer."
  type        = number
  default     = 1001
}

variable "buildkit_service_name" {
  description = "systemd service name for BuildKit on the worker AMI."
  type        = string
  default     = "buildkit"
}

variable "worker_service_name" {
  description = "systemd service name for helmr-worker on the worker AMI."
  type        = string
  default     = "helmr-worker"
}

variable "worker_environment" {
  description = "Additional non-secret environment variables written to the worker env file."
  type        = map(string)
  default     = {}
}

variable "buildkit_slirp_cidr" {
  description = "IPv4 CIDR used by rootlesskit/slirp4netns inside the BuildKit service namespace. It must not overlap network_blocked_ipv4_cidrs."
  type        = string
  default     = "198.18.0.0/24"

  validation {
    condition     = can(cidrnetmask(var.buildkit_slirp_cidr))
    error_message = "buildkit_slirp_cidr must be an IPv4 CIDR prefix."
  }
}

variable "network_blocked_ipv4_cidrs" {
  description = "IPv4 CIDRs blocked from Firecracker task egress. This is the infra-owned baseline policy passed to helmr-worker."
  type        = list(string)
  default = [
    "0.0.0.0/8",
    "10.0.0.0/8",
    "100.64.0.0/10",
    "127.0.0.0/8",
    "169.254.0.0/16",
    "172.16.0.0/12",
    "192.168.0.0/16",
    "224.0.0.0/4",
    "240.0.0.0/4",
  ]

  validation {
    condition = alltrue([
      for cidr in var.network_blocked_ipv4_cidrs :
      can(cidrnetmask(cidr))
    ])
    error_message = "network_blocked_ipv4_cidrs must contain only IPv4 CIDR prefixes."
  }
}

variable "network_blocked_ipv6_cidrs" {
  description = "IPv6 CIDRs blocked from Firecracker task egress. This is the infra-owned baseline policy passed to helmr-worker."
  type        = list(string)
  default = [
    "::/128",
    "::1/128",
    "fc00::/7",
    "fe80::/10",
    "ff00::/8",
  ]
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
