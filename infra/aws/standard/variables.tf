variable "aws_region" {
  description = "AWS region."
  type        = string
}

variable "name" {
  description = "Name prefix for Helmr resources. Keep this short because several AWS resources add suffixes."
  type        = string
  default     = "helmr-standard"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,17}[a-z0-9]$", var.name))
    error_message = "name must be 3-19 characters, start with a lowercase letter, end with a lowercase letter or number, and contain only lowercase letters, numbers, and hyphens."
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

variable "controlplane_vpc_cidr" {
  description = "CIDR block for the unrouted Control Plane VPC."
  type        = string
  default     = "10.90.0.0/16"
}

variable "execution_vpc_cidr" {
  description = "CIDR block for the unrouted Execution VPC. The complete prefix must be covered by the deployment-supplied Worker deny set."
  type        = string
  default     = "10.91.0.0/16"

  validation {
    condition = can(cidrnetmask(var.execution_vpc_cidr)) && anytrue([
      for blocked in var.worker_network_blocked_ipv4_cidrs :
      try(cidrcontains(blocked, cidrhost(var.execution_vpc_cidr, 0)) && cidrcontains(blocked, cidrhost(var.execution_vpc_cidr, -1)), false)
    ])
    error_message = "execution_vpc_cidr must be an IPv4 prefix wholly contained by worker_network_blocked_ipv4_cidrs."
  }
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
  description = "External URL for the Control Plane when enable_cloudfront is false."
  type        = string
  default     = null
  nullable    = true
}

variable "api_origin" {
  description = "External origin used for machine-facing Control Plane API URLs. Defaults to the effective public URL."
  type        = string
  default     = null
  nullable    = true
}

variable "deployment_mode" {
  description = "Helmr deployment mode passed to control-plane tasks."
  type        = string
  default     = "self-hosted"
}

variable "worker_group_name" {
  description = "Name of the initial Worker Group."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9](?:[a-z0-9-]{0,126}[a-z0-9])?$", var.worker_group_name))
    error_message = "worker_group_name must be a lowercase URL-safe identifier of 1-128 characters."
  }
}

variable "region_id" {
  description = "Explicit Helmr region primitive for this stack."
  type        = string

  validation {
    condition = (
      var.region_id != "" &&
      var.region_id == trimspace(var.region_id) &&
      length(base64encode(var.region_id)) <= 340 &&
      length(regexall("[[:cntrl:]]", var.region_id)) == 0
    )
    error_message = "region_id must be normalized control-free UTF-8 of 1-255 bytes."
  }
}

variable "platform_store_uri" {
  description = "Dedicated immutable Platform Artifact store URI exported by the bootstrap module."
  type        = string
}

variable "platform_store_bucket_arn" {
  description = "Platform Artifact store bucket ARN exported by the bootstrap module."
  type        = string
}

variable "platform_store_kms_key_arn" {
  description = "Platform Artifact store KMS key ARN exported by the bootstrap module."
  type        = string
}

variable "build_policy_digest" {
  description = "Exact committed build-policy digest for this stack rollout."
  type        = string

  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.build_policy_digest))
    error_message = "build_policy_digest must be lowercase sha256:<64 hexadecimal digits>."
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
}

variable "clickhouse_password_kms_key_arns" {
  description = "Optional customer-managed KMS key ARNs needed to decrypt clickhouse_password_secret_arn."
  type        = list(string)
  default     = []
}

variable "additional_controlplane_security_group_ids" {
  description = "Additional security groups attached to controlplane, dispatcher, and migration tasks."
  type        = list(string)
  default     = []
}

variable "cloudfront_origin_domain_name" {
  description = "DNS name CloudFront uses for the HTTPS ALB origin when enable_cloudfront is true. This name must resolve to the public ALB and be covered by certificate_arn."
  type        = string
  default     = null
  nullable    = true
}

variable "helmr_version" {
  description = "Helmr release version to deploy, for example vX.Y.Z. Used to resolve official controlplane and worker artifacts."
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

variable "controlplane_image" {
  description = "Optional digest-pinned controlplane image URI override for custom builds. When null, the release artifact manifest is used."
  type        = string
  default     = null
  nullable    = true
}

variable "controlplane_image_repository_arn" {
  description = "Exact private ECR repository ARN needed by the ECS execution roles. Leave null for public or non-ECR images."
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

variable "create_controlplane_service" {
  description = "Create the ECS service after image, secrets, and migrations are ready."
  type        = bool
  default     = false
}

variable "controlplane_desired_count" {
  description = "Desired ECS task count for the controlplane service."
  type        = number
  default     = 2
}

variable "dispatcher_desired_count" {
  description = "Desired ECS task count for helmr-dispatcher."
  type        = number
  default     = 1
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

variable "worker_launch_timeout_seconds" {
  description = "Deployment-owned ASG launch-hook timeout while a Worker reaches Control Plane readiness."
  type        = number
  default     = 900

  validation {
    condition     = var.worker_launch_timeout_seconds > 30
    error_message = "worker_launch_timeout_seconds must exceed the Worker lifecycle heartbeat interval."
  }
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

variable "worker_network_blocked_ipv4_cidrs" {
  description = "Canonical ordered IPv4 CIDRs blocked for all guest egress. Supply [] only when the deployment intentionally has no additional destination deny."
  type        = list(string)

  validation {
    condition = length(distinct(var.worker_network_blocked_ipv4_cidrs)) == length(var.worker_network_blocked_ipv4_cidrs) && alltrue([
      for cidr in var.worker_network_blocked_ipv4_cidrs : can(cidrnetmask(cidr)) && try(cidrhost(cidr, 0) == split("/", cidr)[0], false)
    ])
    error_message = "worker_network_blocked_ipv4_cidrs must contain unique canonical IPv4 CIDRs."
  }
}

variable "worker_network_link_pool" {
  description = "Worker-local IPv4 pool used for routed veth links."
  type        = string
  default     = "169.254.64.0/18"
}

variable "worker_network_translation_pool" {
  description = "Worker-local IPv4 pool used for routed guest translation identities."
  type        = string
  default     = "100.96.0.0/16"
}

variable "worker_network_resolver_ipv4" {
  description = "Exact IPv4 resolver exposed to guests. Null selects the VPC resolver."
  type        = string
  default     = null
  nullable    = true
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
  default  = 3
  nullable = true
}
variable "build_worker_vm_memory_mib" {
  type     = number
  default  = 4096
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
