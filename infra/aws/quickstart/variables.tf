variable "aws_region" {
  description = "AWS region for the quickstart deployment."
  type        = string
}

variable "name" {
  description = "Name prefix for Helmr resources. Keep this short because several AWS resources add suffixes."
  type        = string
  default     = "helmr-quickstart"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,17}[a-z0-9]$", var.name))
    error_message = "name must be 3-19 characters, start with a lowercase letter, end with a lowercase letter or number, and contain only lowercase letters, numbers, and hyphens."
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

variable "app_origin" {
  description = "Public HTTPS origin. Point its DNS A record directly at controlplane_public_ip."
  type        = string
  validation {
    condition     = can(regex("^https://[a-z0-9]([a-z0-9.-]*[a-z0-9])?$", var.app_origin))
    error_message = "app_origin must be a public DNS HTTPS origin without a path or custom port."
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

variable "helmr_version" {
  description = "Exact Helmr release tag to deploy, vX.Y.Z or vX.Y.Z-prerelease. Used to resolve official controlplane and worker artifacts."
  type        = string

  validation {
    condition     = can(regex("^v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)([.]((0|[1-9][0-9]*)|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?$", var.helmr_version))
    error_message = "helmr_version must be an exact vX.Y.Z or vX.Y.Z-prerelease tag."
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

variable "create_worker" {
  description = "Create worker EC2 Auto Scaling resources. Defaults off; enable for a single nested-virtualization smoke worker."
  type        = bool
  default     = false
}

variable "worker_generation_key" {
  description = "Operator-selected immutable generation key. Change only to create a fresh same-build Pool/provider binding for an explicitly qualified recovery; retain the old definition first."
  type        = string
  default     = "initial"
  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{0,63}$", var.worker_generation_key))
    error_message = "worker_generation_key must be 1-64 lowercase letters, digits or hyphens, starting with a letter or digit."
  }
}

variable "retained_worker_generations" {
  description = "Immutable prior execution Worker Pool generations retained with explicit capacity and their exact ASG/LT/AMI definition for exact checkpoint restore. Keys must equal execution-sha256(jsonencode(generation_inputs))."
  type = map(object({
    generation_inputs = object({
      generation_key        = string
      ami_id                = string
      instance_type         = string
      nested_virtualization = bool
      supply = object({
        computer = object({
          devices = list(string)
        })
        environment     = map(string)
        contract_digest = string
        enable_ssm      = bool
        network = object({
          blocked_ipv4_cidrs     = list(string)
          private_business_cidrs = list(string)
          link_pool              = string
          resolver_ipv4          = string
          translation_pool       = string
        })
        platform_store = object({
          uri         = string
          bucket_arn  = string
          kms_key_arn = string
        })
        root_volume = object({
          size_gb    = number
          iops       = number
          throughput = number
        })
        disk = object({
          total_mib            = number
          reserve_mib          = number
          computer_staging_mib = number
        })
        lifecycle = object({
          health_check_grace_period_seconds               = number
          launch_lifecycle_heartbeat_timeout_seconds      = number
          termination_lifecycle_heartbeat_timeout_seconds = number
          termination_wait_timeout_seconds                = number
          lifecycle_heartbeat_interval_seconds            = number
          termination_policies                            = list(string)
          protect_from_scale_in                           = bool
          health_check_type                               = string
          launch_lifecycle_transition                     = string
          launch_lifecycle_default_result                 = string
          termination_lifecycle_transition                = string
          termination_lifecycle_default_result            = string
        })
      })
      capacity = object({
        cpu_millis               = number
        memory_mib               = number
        guest_ephemeral_disk_mib = number
        vm_slots                 = number
      })
      per_vm = object({
        cpu_millis               = number
        memory_mib               = number
        guest_ephemeral_disk_mib = number
      })
    })
    count = number
    sealed_provider_definition = object({
      user_data_base64                                = string
      permission_policy_json                          = string
      boundary_policy_arn                             = string
      boundary_policy_json                            = string
      enable_ssm                                      = bool
      launch_template_version                         = string
      health_check_grace_period_seconds               = number
      launch_lifecycle_heartbeat_timeout_seconds      = number
      termination_lifecycle_heartbeat_timeout_seconds = number
      termination_wait_timeout_seconds                = number
      lifecycle_heartbeat_interval_seconds            = number
      termination_policies                            = list(string)
      protect_from_scale_in                           = bool
      health_check_type                               = string
      launch_lifecycle_transition                     = string
      launch_lifecycle_default_result                 = string
      termination_lifecycle_transition                = string
      termination_lifecycle_default_result            = string
    })
  }))
  default = {}

  validation {
    condition = alltrue([
      for pool_name, generation in var.retained_worker_generations :
      pool_name == "execution-${sha256(jsonencode(generation.generation_inputs))}" &&
      generation.count >= 0 && floor(generation.count) == generation.count &&
      generation.generation_inputs.supply.disk.total_mib > generation.generation_inputs.supply.disk.reserve_mib &&
      generation.generation_inputs.capacity.cpu_millis > 0 &&
      generation.generation_inputs.capacity.cpu_millis % 1000 == 0 &&
      generation.generation_inputs.capacity.memory_mib > 0 &&
      generation.generation_inputs.capacity.vm_slots > 0 &&
      generation.generation_inputs.per_vm.cpu_millis > 0 &&
      generation.generation_inputs.per_vm.cpu_millis % 1000 == 0 &&
      generation.generation_inputs.per_vm.memory_mib > 0 &&
      generation.generation_inputs.per_vm.guest_ephemeral_disk_mib > 0 &&
      generation.generation_inputs.supply.disk.computer_staging_mib > 0 &&
      generation.generation_inputs.capacity.guest_ephemeral_disk_mib == generation.generation_inputs.capacity.vm_slots * generation.generation_inputs.per_vm.guest_ephemeral_disk_mib &&
      can(base64decode(generation.sealed_provider_definition.user_data_base64)) &&
      can(jsondecode(generation.sealed_provider_definition.permission_policy_json)) &&
      can(jsondecode(generation.sealed_provider_definition.boundary_policy_json)) &&
      can(regex("^[1-9][0-9]*$", generation.sealed_provider_definition.launch_template_version)) &&
      generation.sealed_provider_definition.enable_ssm == generation.generation_inputs.supply.enable_ssm &&
      generation.sealed_provider_definition.health_check_grace_period_seconds == generation.generation_inputs.supply.lifecycle.health_check_grace_period_seconds &&
      generation.sealed_provider_definition.launch_lifecycle_heartbeat_timeout_seconds == generation.generation_inputs.supply.lifecycle.launch_lifecycle_heartbeat_timeout_seconds &&
      generation.sealed_provider_definition.termination_lifecycle_heartbeat_timeout_seconds == generation.generation_inputs.supply.lifecycle.termination_lifecycle_heartbeat_timeout_seconds &&
      generation.sealed_provider_definition.termination_wait_timeout_seconds == generation.generation_inputs.supply.lifecycle.termination_wait_timeout_seconds &&
      generation.sealed_provider_definition.lifecycle_heartbeat_interval_seconds == generation.generation_inputs.supply.lifecycle.lifecycle_heartbeat_interval_seconds &&
      generation.sealed_provider_definition.termination_policies == generation.generation_inputs.supply.lifecycle.termination_policies &&
      generation.sealed_provider_definition.protect_from_scale_in == generation.generation_inputs.supply.lifecycle.protect_from_scale_in &&
      generation.sealed_provider_definition.health_check_type == generation.generation_inputs.supply.lifecycle.health_check_type &&
      generation.sealed_provider_definition.launch_lifecycle_transition == generation.generation_inputs.supply.lifecycle.launch_lifecycle_transition &&
      generation.sealed_provider_definition.launch_lifecycle_default_result == generation.generation_inputs.supply.lifecycle.launch_lifecycle_default_result &&
      generation.sealed_provider_definition.termination_lifecycle_transition == generation.generation_inputs.supply.lifecycle.termination_lifecycle_transition &&
      generation.sealed_provider_definition.termination_lifecycle_default_result == generation.generation_inputs.supply.lifecycle.termination_lifecycle_default_result &&
      generation.sealed_provider_definition.health_check_grace_period_seconds > 0 &&
      generation.sealed_provider_definition.launch_lifecycle_heartbeat_timeout_seconds > generation.sealed_provider_definition.lifecycle_heartbeat_interval_seconds &&
      generation.sealed_provider_definition.termination_lifecycle_heartbeat_timeout_seconds >= generation.sealed_provider_definition.lifecycle_heartbeat_interval_seconds * 3 &&
      generation.sealed_provider_definition.termination_wait_timeout_seconds > 0 &&
      length(generation.sealed_provider_definition.termination_policies) > 0
    ])
    error_message = "retained_worker_generations must bind each canonical execution Pool key to one complete generation input with a non-negative whole count and sealed provider definition."
  }
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

variable "worker_ami_id" {
  description = "Optional worker AMI override for custom builds. When null and create_worker is true, the release artifact manifest is used."
  type        = string
  default     = null
  nullable    = true
}

variable "worker_host_type" {
  description = "EC2 instance type for the smoke worker."
  type        = string
  default     = "c8i.large"
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

variable "worker_network_blocked_ipv4_cidrs" {
  default     = []
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

variable "worker_count" {
  description = "Explicit fixed count for the current Worker generation; required when create_worker is true. Use zero to prepare an inert target or deliberately stop capacity after draining."
  type        = number
  default     = 1
  validation {
    condition     = var.worker_count == null ? !var.create_worker : var.worker_count >= 0 && floor(var.worker_count) == var.worker_count
    error_message = "Set worker_count to a non-negative whole number when creating Worker resources."
  }
}

variable "worker_capacity_vcpus" {
  type     = number
  default  = 1
  nullable = true
}
variable "worker_capacity_memory_mib" {
  type     = number
  default  = 2560
  nullable = true
}
variable "worker_execution_slots" {
  type     = number
  default  = 1
  nullable = true
}


variable "worker_root_volume_size_gb" {
  description = "Smoke worker root EBS volume size in GiB."
  type        = number
  default     = 320
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
  default     = 311296
  nullable    = true
}

variable "worker_disk_reserve_mib" {
  description = "Filesystem capacity in MiB withheld from advertised worker capacity."
  type        = number
  default     = 8192

  validation {
    condition     = var.worker_disk_reserve_mib > 0
    error_message = "worker_disk_reserve_mib must be positive."
  }
}

variable "worker_vm_vcpus" {
  description = "vCPU count assigned to each worker Firecracker task VM."
  type        = number
  default     = 1
}

variable "worker_vm_memory_mib" {
  description = "Memory in MiB assigned to each worker Firecracker task VM."
  type        = number
  default     = 2048
}

variable "worker_vm_scratch_disk_mib" {
  description = "Writable disk in MiB assigned to each worker Firecracker task VM."
  type        = number
  default     = 32768
}

variable "worker_computer_devices" {
  default     = ["/dev/nbd0"]
  description = "Exclusive Worker NBD device allowlist."
  type        = list(string)
}

variable "worker_computer_staging_mib" {
  description = "Per-Instance Computer storage staging budget in MiB. Changing it creates a new Worker generation."
  type        = number
  default     = 65536
  validation {
    condition     = var.worker_computer_staging_mib > 0 && floor(var.worker_computer_staging_mib) == var.worker_computer_staging_mib && var.worker_computer_staging_mib <= 8796093022207
    error_message = "worker_computer_staging_mib must be positive whole MiB representable in int64 bytes."
  }
}

variable "worker_network_private_business_cidrs" {
  description = "Installation RFC1918 business ceiling, applied to both control plane and Workers. Routing and TLS trust remain separately configured."
  type        = list(string)
  default     = []
  validation {
    condition     = length(distinct(var.worker_network_private_business_cidrs)) == length(var.worker_network_private_business_cidrs) && alltrue([for cidr in var.worker_network_private_business_cidrs : can(cidrnetmask(cidr)) && try(cidrhost(cidr, 0) == split("/", cidr)[0], false)])
    error_message = "worker_network_private_business_cidrs must contain unique canonical IPv4 CIDRs; modules additionally require RFC1918 and reject deny overlap."
  }
}

variable "worker_environment" {
  description = "Additional non-secret Worker environment, including explicit WORKER_LOG_CHUNK_BYTES, WORKER_LOG_BUFFER_BYTES and WORKER_LOG_BUFFER_RECORDS. Stored in immutable Pool generation inputs; changes create a new generation."
  type        = map(string)
  default = {
    WORKER_LOG_CHUNK_BYTES    = "65536"
    WORKER_LOG_BUFFER_BYTES   = "1048576"
    WORKER_LOG_BUFFER_RECORDS = "256"
  }
}

variable "vpc_cidr" {
  description = "Quickstart VPC range; its first /24 is the public host subnet."
  type        = string
  default     = "10.80.0.0/16"
  validation {
    condition     = can(cidrnetmask(var.vpc_cidr)) && try(split("/", var.vpc_cidr)[1] == "16" && cidrhost(var.vpc_cidr, 0) == split("/", var.vpc_cidr)[0], false)
    error_message = "vpc_cidr must be a canonical IPv4 /16 network."
  }
}

variable "control_ami_id" {
  description = "Ubuntu 24.04 LTS amd64 AMI with cloud-init and SSM Agent, in aws_region."
  type        = string
  validation {
    condition     = can(regex("^ami-[0-9a-f]{8,}$", var.control_ami_id))
    error_message = "Supply a regional Ubuntu 24.04 amd64 AMI ID."
  }
}

variable "control_instance_type" {
  description = "Control host instance type."
  type        = string
  default     = "t3.large"
}

variable "control_disk_gb" {
  description = "Control host disk for PostgreSQL, telemetry and TLS state. No backup; deleted on termination."
  type        = number
  default     = 60
}

variable "github_oauth_client_id" {
  description = "GitHub OAuth application client ID; empty disables GitHub login."
  type        = string
  default     = ""
}

variable "region_id" {
  description = "Logical Helmr Region ID."
  type        = string
}

variable "worker_group_name" {
  description = "Initial logical Worker Group."
  type        = string
  default     = "default"
}

variable "slack_transport" {
  description = "Slack transport: socket, http, or disabled."
  type        = string
  default     = "socket"
  validation {
    condition     = contains(["socket", "http", "disabled"], var.slack_transport)
    error_message = "slack_transport must be socket, http or disabled."
  }
}

variable "controlplane_environment" {
  description = "Additional non-secret Control Plane configuration."
  type        = map(string)
  default     = {}
}

variable "dispatcher_environment" {
  description = "Additional non-secret Dispatcher configuration."
  type        = map(string)
  default     = {}
}

variable "postgres_image" {
  description = "PostgreSQL 18 container image. Pin a digest for reproducible deployment."
  type        = string
  default     = "postgres:18-bookworm@sha256:5c4783a8cc3f0074912164f94dfa1be4e043438f0f707cf968d7af9a0d31a2c8"
}

variable "clickhouse_image" {
  description = "ClickHouse server image matching the supported telemetry schema."
  type        = string
  default     = "clickhouse/clickhouse-server:26.7.5.10@sha256:800e82865530eb2f1c4bc1b960e43b435fd9b2d83b4bd04a2564a5cfd88fdb6e"
}

variable "caddy_image" {
  description = "Caddy image for direct HTTPS and automatic certificate renewal."
  type        = string
  default     = "caddy:2@sha256:5e5286cb37d44086d651a5363f05f43a692ce4d649d88fb1bd844579f45fa9e8"
}
