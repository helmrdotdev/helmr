variable "name" {
  description = "Name prefix for worker resources."
  type        = string
}

variable "worker_roles" {
  description = "Roles this worker group is permitted to advertise."
  type        = set(string)
  default     = ["run", "build"]

  validation {
    condition     = length(var.worker_roles) > 0 && length(setsubtract(var.worker_roles, ["run", "build"])) == 0
    error_message = "worker_roles must contain run, build, or both."
  }
}

variable "worker_pool_name" {
  description = "Canonical logical Worker Pool generation name advertised during enrollment."
  type        = string

  validation {
    condition = (
      length(var.worker_pool_name) >= 1 &&
      length(var.worker_pool_name) <= 128 &&
      can(regex("^[a-z0-9]([a-z0-9-]{0,126}[a-z0-9])?$", var.worker_pool_name))
    )
    error_message = "worker_pool_name must be a lowercase identifier of 1 to 128 letters, digits, or internal hyphens."
  }
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
  description = "Worker AMI with Firecracker, jailer, routed-TAP prerequisites, and helmr-worker installed."
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

variable "sealed_provider_definition" {
  description = "Exact realized provider authority retained for an existing immutable Worker Pool. Null creates the current definition; a value preserves its user data, IAM policies, SSM contract, and launch-template version while the Pool remains restore-capable."
  type = object({
    user_data_base64                                = string
    permission_policy_json                          = string
    boundary_policy_json                            = string
    enable_ssm                                      = bool
    launch_template_version                         = string
    health_check_grace_period_seconds               = number
    launch_lifecycle_heartbeat_timeout_seconds      = number
    termination_lifecycle_heartbeat_timeout_seconds = number
    termination_drain_timeout_seconds               = number
    lifecycle_heartbeat_interval_seconds            = number
    termination_policies                            = list(string)
    protect_from_scale_in                           = bool
    health_check_type                               = string
    instance_refresh_strategy                       = string
    instance_refresh_min_healthy_percentage         = number
    instance_refresh_max_healthy_percentage         = number
    instance_refresh_scale_in_protected_instances   = string
    instance_refresh_standby_instances              = string
    instance_refresh_skip_matching                  = bool
    launch_lifecycle_transition                     = string
    launch_lifecycle_default_result                 = string
    termination_lifecycle_transition                = string
    termination_lifecycle_default_result            = string
  })
  default  = null
  nullable = true

  validation {
    condition = var.sealed_provider_definition == null || (
      can(base64decode(var.sealed_provider_definition.user_data_base64)) &&
      can(jsondecode(var.sealed_provider_definition.permission_policy_json)) &&
      can(jsondecode(var.sealed_provider_definition.boundary_policy_json)) &&
      can(regex("^[1-9][0-9]*$", var.sealed_provider_definition.launch_template_version)) &&
      var.sealed_provider_definition.health_check_grace_period_seconds > 0 &&
      var.sealed_provider_definition.launch_lifecycle_heartbeat_timeout_seconds > var.sealed_provider_definition.lifecycle_heartbeat_interval_seconds &&
      var.sealed_provider_definition.termination_lifecycle_heartbeat_timeout_seconds >= var.sealed_provider_definition.lifecycle_heartbeat_interval_seconds * 3 &&
      var.sealed_provider_definition.termination_drain_timeout_seconds > 0 &&
      length(var.sealed_provider_definition.termination_policies) > 0 &&
      contains(["EC2", "ELB", "VPC_LATTICE"], var.sealed_provider_definition.health_check_type) &&
      var.sealed_provider_definition.instance_refresh_strategy == "Rolling" &&
      var.sealed_provider_definition.instance_refresh_min_healthy_percentage >= 0 &&
      var.sealed_provider_definition.instance_refresh_max_healthy_percentage >= 100 &&
      contains(["Refresh", "Ignore", "Wait"], var.sealed_provider_definition.instance_refresh_scale_in_protected_instances) &&
      contains(["Terminate", "Ignore", "Wait"], var.sealed_provider_definition.instance_refresh_standby_instances) &&
      var.sealed_provider_definition.launch_lifecycle_transition == "autoscaling:EC2_INSTANCE_LAUNCHING" &&
      contains(["ABANDON", "CONTINUE"], var.sealed_provider_definition.launch_lifecycle_default_result) &&
      var.sealed_provider_definition.termination_lifecycle_transition == "autoscaling:EC2_INSTANCE_TERMINATING" &&
      contains(["ABANDON", "CONTINUE"], var.sealed_provider_definition.termination_lifecycle_default_result)
    )
    error_message = "sealed_provider_definition must contain valid immutable user data, IAM, launch-template, and ASG lifecycle authority."
  }
}

variable "min_size" {
  description = "Minimum worker instance count."
  type        = number
  default     = 0
}

variable "max_size" {
  description = "Maximum worker instance count."
  type        = number
  default     = 3
}

variable "health_check_grace_period_seconds" {
  description = "ASG health check grace period for worker instances."
  type        = number
  default     = 900
}

variable "launch_lifecycle_heartbeat_timeout_seconds" {
  description = "Seconds to wait for worker instance bootstrap before the launch lifecycle hook times out."
  type        = number
  default     = 900

  validation {
    condition     = var.launch_lifecycle_heartbeat_timeout_seconds > var.lifecycle_heartbeat_interval_seconds
    error_message = "launch lifecycle timeout must exceed the heartbeat interval."
  }
}

variable "termination_lifecycle_heartbeat_timeout_seconds" {
  description = "Seconds to wait for worker drain before the termination lifecycle hook times out."
  type        = number
  default     = 180

  validation {
    condition     = var.termination_lifecycle_heartbeat_timeout_seconds >= var.lifecycle_heartbeat_interval_seconds * 3
    error_message = "termination lifecycle timeout must be at least three heartbeat intervals."
  }
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

variable "worker_disk_mib" {
  description = "Optional filesystem capacity ceiling in MiB before worker_disk_reserve_mib is withheld. When null, helmr-worker detects local filesystem capacity."
  type        = number
  default     = null
  nullable    = true
}

variable "worker_disk_reserve_mib" {
  description = "Filesystem capacity in MiB withheld from advertised workload, scratch, and cache capacity."
  type        = number
  default     = 1024

  validation {
    condition     = var.worker_disk_reserve_mib > 0
    error_message = "worker_disk_reserve_mib must be positive."
  }
}

variable "vm_vcpus" {
  description = "Maximum vCPU count assigned to one Firecracker VM."
  type        = number
  default     = 2

  validation {
    condition     = var.vm_vcpus > 0
    error_message = "vm_vcpus must be positive."
  }
}

variable "vm_memory_mib" {
  description = "Maximum memory in MiB assigned to one Firecracker VM."
  type        = number
  default     = 4096

  validation {
    condition     = var.vm_memory_mib > 0
    error_message = "vm_memory_mib must be positive."
  }
}

variable "vm_scratch_disk_mib" {
  description = "Writable scratch disk in MiB attached to each Firecracker task VM and used for product-managed runtime staging."
  type        = number
  default     = 8192

  validation {
    condition     = var.vm_scratch_disk_mib > 0
    error_message = "vm_scratch_disk_mib must be positive."
  }
}

variable "worker_capacity_vcpus" {
  description = "Worker host vCPU pool after kernel and supervisor reserves."
  type        = number
  default     = null
  nullable    = true

  validation {
    condition     = var.worker_capacity_vcpus == null || var.worker_capacity_vcpus >= var.vm_vcpus
    error_message = "worker_capacity_vcpus must be null or at least vm_vcpus."
  }
}

variable "worker_capacity_memory_mib" {
  description = "Worker host memory pool after kernel and supervisor reserves."
  type        = number
  default     = null
  nullable    = true

  validation {
    condition     = var.worker_capacity_memory_mib == null || var.worker_capacity_memory_mib >= var.vm_memory_mib
    error_message = "worker_capacity_memory_mib must be null or at least vm_memory_mib."
  }
}

variable "worker_execution_slots" {
  description = "Maximum concurrent Firecracker VM slots. Build execution has an independent fixed single-executor limit."
  type        = number
  default     = null
  nullable    = true

  validation {
    condition     = var.worker_execution_slots == null || var.worker_execution_slots > 0
    error_message = "worker_execution_slots must be null or positive."
  }
}

variable "substrate_cache_max_mib" {
  description = "Optional maximum substrate cache size in MiB. Set explicitly when the VM disk shape and host volume leave less room than the derived cache budget."
  type        = number
  default     = null
  nullable    = true

  validation {
    condition     = var.substrate_cache_max_mib == null || var.substrate_cache_max_mib > 0
    error_message = "substrate_cache_max_mib must be null or positive."
  }
}

variable "artifact_cache_max_mib" {
  description = "Optional maximum artifact cache size in MiB. Set explicitly when the VM disk shape and host volume leave less room than the derived cache budget."
  type        = number
  default     = null
  nullable    = true

  validation {
    condition     = var.artifact_cache_max_mib == null || var.artifact_cache_max_mib > 0
    error_message = "artifact_cache_max_mib must be null or positive."
  }
}

variable "build_cache_mib" {
  description = "Usable MiB allocated to the physically isolated build-cache filesystem."
  type        = number
  default     = null
  nullable    = true

  validation {
    condition     = var.build_cache_mib == null || var.build_cache_mib > 0
    error_message = "build_cache_mib must be null or positive."
  }
}

variable "build_scratch_mib" {
  description = "Usable MiB allocated to the physically isolated build-scratch filesystem."
  type        = number
  default     = null
  nullable    = true

  validation {
    condition     = var.build_scratch_mib == null || var.build_scratch_mib > 0
    error_message = "build_scratch_mib must be null or positive."
  }
}

variable "worker_controlplane_url" {
  description = "Worker-facing control-plane API URL for CONTROL_PLANE_URL. Prefer a private DNS name that matches the HTTPS certificate."
  type        = string
}

variable "cas_uri" {
  description = "CAS URI for CAS_URI."
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

variable "build_policy_digest" {
  description = "Exact build-policy digest installed by build-capable workers; must be null for run-only workers."
  type        = string
  nullable    = true

  validation {
    condition     = var.build_policy_digest == null || can(regex("^sha256:[0-9a-f]{64}$", var.build_policy_digest))
    error_message = "build_policy_digest must be null or lowercase sha256:<64 hexadecimal digits>."
  }
}

variable "secret_arns" {
  description = "Secret ARNs required by the worker."
  type = object({
    checkpoint_encryption_key = string
    worker_enrollment_token   = string
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

variable "worker_service_name" {
  description = "systemd service name for helmr-worker on the worker AMI."
  type        = string
  default     = "helmr-worker"
}

variable "worker_environment" {
  description = "Additional non-secret environment variables written to the Worker env file. Keys managed by this module remain reserved even when their values are conditionally absent."
  type        = map(string)
  default     = {}
}

variable "network_blocked_ipv4_cidrs" {
  description = "Canonical IPv4 CIDRs added to the Worker-wide guest destination deny set. Use an explicit empty list for no additional deny."
  type        = list(string)

  validation {
    condition = length(distinct(var.network_blocked_ipv4_cidrs)) == length(var.network_blocked_ipv4_cidrs) && alltrue([
      for cidr in var.network_blocked_ipv4_cidrs : can(cidrnetmask(cidr)) && try(cidrhost(cidr, 0) == split("/", cidr)[0], false)
    ])
    error_message = "network_blocked_ipv4_cidrs must contain unique canonical IPv4 CIDRs."
  }
}

variable "network_link_pool" {
  description = "Canonical IPv4 pool used to allocate one host/namespace veth /31 per concurrent VM."
  type        = string

  validation {
    condition     = can(cidrnetmask(var.network_link_pool)) && try(cidrhost(var.network_link_pool, 0) == split("/", var.network_link_pool)[0], false)
    error_message = "network_link_pool must be a canonical IPv4 CIDR."
  }
}

variable "network_translation_pool" {
  description = "Canonical IPv4 pool used to allocate one routed translation address per concurrent VM."
  type        = string

  validation {
    condition     = can(cidrnetmask(var.network_translation_pool)) && try(cidrhost(var.network_translation_pool, 0) == split("/", var.network_translation_pool)[0], false)
    error_message = "network_translation_pool must be a canonical IPv4 CIDR."
  }
}

variable "network_resolver_ipv4" {
  description = "Optional exact IPv4 resolver exposed to guests. Null selects the VPC resolver."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition = var.network_resolver_ipv4 == null || (
      can(cidrnetmask("${var.network_resolver_ipv4}/32")) &&
      try(cidrhost("${var.network_resolver_ipv4}/32", 0) == var.network_resolver_ipv4, false)
    )
    error_message = "network_resolver_ipv4 must be an IPv4 address when set."
  }
}

variable "image_cache_registry_authority" {
  description = "Canonical regional ECR registry authority for the Platform image cache."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}\\.dkr\\.ecr\\.[a-z0-9-]+\\.amazonaws\\.com(\\.cn)?$", var.image_cache_registry_authority))
    error_message = "image_cache_registry_authority must be a canonical private ECR registry authority."
  }
}

variable "image_cache_repository_prefix" {
  description = "Bounded ECR repository namespace for Environment image caches."
  type        = string
}

variable "image_cache_role_arn" {
  description = "Exact regional Execution image-cache role ARN."
  type        = string
}

variable "image_cache_repository_arn_prefix" {
  description = "Exact ECR repository ARN prefix matching image_cache_repository_prefix."
  type        = string
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
