locals {
  name                    = lower(var.name)
  worker_controlplane_url = module.controlplane.controlplane_url
  worker_ami_id           = coalesce(var.worker_ami_id, module.release_artifacts.worker_ami_id, "ami-unconfigured")
  worker_provider_contract_digest = sha256(jsonencode({
    module   = filesha256("${path.module}/../modules/worker/main.tf")
    inputs   = filesha256("${path.module}/../modules/worker/variables.tf")
    userdata = filesha256("${path.module}/../modules/worker/templates/user-data.sh.tftpl")
  }))

  worker_substrate_cache_mib      = coalesce(var.worker_substrate_cache_max_mib, 0)
  worker_artifact_cache_mib       = coalesce(var.worker_artifact_cache_max_mib, 0)
  worker_shared_disk_mib          = coalesce(var.worker_disk_mib, 0) - var.worker_disk_reserve_mib - local.worker_substrate_cache_mib - local.worker_artifact_cache_mib
  worker_guest_ephemeral_disk_mib = local.worker_shared_disk_mib
  worker_generation_inputs = {
    ami_id                = local.worker_ami_id
    instance_type         = var.worker_instance_type
    nested_virtualization = var.worker_enable_nested_virtualization
    supply = {
      contract_digest = local.worker_provider_contract_digest
      enable_ssm      = var.worker_enable_ssm
      network = {
        blocked_ipv4_cidrs = var.worker_network_blocked_ipv4_cidrs
        link_pool          = var.worker_network_link_pool
        resolver_ipv4      = coalesce(var.worker_network_resolver_ipv4, cidrhost(var.execution_vpc_cidr, 2))
        translation_pool   = var.worker_network_translation_pool
      }
      platform_store = {
        uri         = var.platform_store_uri
        bucket_arn  = var.platform_store_bucket_arn
        kms_key_arn = var.platform_store_kms_key_arn
      }
      root_volume = {
        size_gb    = var.worker_root_volume_size_gb
        iops       = var.worker_root_volume_iops
        throughput = var.worker_root_volume_throughput
      }
      disk = {
        total_mib           = var.worker_disk_mib
        reserve_mib         = var.worker_disk_reserve_mib
        substrate_cache_mib = var.worker_substrate_cache_max_mib
        artifact_cache_mib  = var.worker_artifact_cache_max_mib
      }
      lifecycle = {
        health_check_grace_period_seconds               = 900
        launch_lifecycle_heartbeat_timeout_seconds      = var.worker_launch_timeout_seconds
        termination_lifecycle_heartbeat_timeout_seconds = 180
        termination_drain_timeout_seconds               = 1800
        lifecycle_heartbeat_interval_seconds            = 60
        termination_policies                            = ["OldestLaunchTemplate", "OldestInstance"]
        protect_from_scale_in                           = true
        health_check_type                               = "EC2"
        instance_refresh_strategy                       = "Rolling"
        instance_refresh_min_healthy_percentage         = 100
        instance_refresh_max_healthy_percentage         = 100
        instance_refresh_scale_in_protected_instances   = "Refresh"
        instance_refresh_standby_instances              = "Terminate"
        instance_refresh_skip_matching                  = true
        launch_lifecycle_transition                     = "autoscaling:EC2_INSTANCE_LAUNCHING"
        launch_lifecycle_default_result                 = "ABANDON"
        termination_lifecycle_transition                = "autoscaling:EC2_INSTANCE_TERMINATING"
        termination_lifecycle_default_result            = "CONTINUE"
      }
    }
    capacity = {
      cpu_millis               = coalesce(var.worker_capacity_vcpus, 0) * 1000
      memory_mib               = coalesce(var.worker_capacity_memory_mib, 0)
      guest_ephemeral_disk_mib = local.worker_guest_ephemeral_disk_mib
      vm_slots                 = coalesce(var.worker_execution_slots, 0)
    }
    per_vm = {
      cpu_millis               = var.worker_vm_vcpus * 1000
      memory_mib               = var.worker_vm_memory_mib
      guest_ephemeral_disk_mib = var.worker_vm_scratch_disk_mib
    }
  }
  worker_pool_name = "execution-${sha256(jsonencode(local.worker_generation_inputs))}"
  current_worker_generation_specs = {
    (local.worker_pool_name) = {
      generation_inputs          = local.worker_generation_inputs
      min_size                   = var.worker_min_size
      max_size                   = var.worker_max_size
      sealed_provider_definition = null
    }
  }
  worker_generation_specs = merge(var.retained_worker_generations, local.current_worker_generation_specs)
  worker_generations = {
    for pool_name, spec in local.worker_generation_specs :
    pool_name => jsondecode(jsonencode({
      generation_inputs            = spec.generation_inputs
      provider_name                = "${local.name}-worker-${substr(sha256(pool_name), 0, 31)}"
      ami_id                       = spec.generation_inputs.ami_id
      instance_type                = spec.generation_inputs.instance_type
      enable_nested_virtualization = spec.generation_inputs.nested_virtualization
      min_size                     = spec.min_size
      max_size                     = spec.max_size
      root_volume_size_gb          = spec.generation_inputs.supply.root_volume.size_gb
      root_volume_iops             = spec.generation_inputs.supply.root_volume.iops
      root_volume_throughput       = spec.generation_inputs.supply.root_volume.throughput
      worker_disk_mib              = spec.generation_inputs.supply.disk.total_mib
      worker_disk_reserve_mib      = spec.generation_inputs.supply.disk.reserve_mib
      vm_vcpus                     = spec.generation_inputs.per_vm.cpu_millis / 1000
      vm_memory_mib                = spec.generation_inputs.per_vm.memory_mib
      vm_scratch_disk_mib          = spec.generation_inputs.per_vm.guest_ephemeral_disk_mib
      worker_capacity_vcpus        = spec.generation_inputs.capacity.cpu_millis / 1000
      worker_capacity_memory_mib   = spec.generation_inputs.capacity.memory_mib
      worker_execution_slots       = spec.generation_inputs.capacity.vm_slots
      substrate_cache_max_mib      = spec.generation_inputs.supply.disk.substrate_cache_mib
      artifact_cache_max_mib       = spec.generation_inputs.supply.disk.artifact_cache_mib
      lifecycle                    = spec.generation_inputs.supply.lifecycle
      sealed_provider_definition   = spec.sealed_provider_definition
    }))
  }

  tags = merge({
    Project     = "helmr"
    Application = "helmr"
    Environment = var.environment
    Example     = "quickstart"
    ManagedBy   = "terraform"
  }, var.tags)
}

module "controlplane_network" {
  source = "../modules/network"

  name                    = "${local.name}-controlplane"
  vpc_cidr                = var.controlplane_vpc_cidr
  availability_zone_count = var.availability_zone_count
  enable_nat_gateway      = var.enable_nat_gateway
  tags                    = local.tags
}

module "execution_network" {
  source = "../modules/network"

  name                    = "${local.name}-execution"
  vpc_cidr                = var.execution_vpc_cidr
  availability_zone_count = var.availability_zone_count
  enable_nat_gateway      = var.enable_nat_gateway
  tags                    = local.tags
}

module "release_artifacts" {
  source = "../modules/release-artifacts"

  helmr_version               = var.helmr_version
  aws_region                  = var.aws_region
  manifest_base_url           = var.release_artifacts_manifest_base_url
  manifest_url                = var.release_artifacts_manifest_url
  controlplane_image_override = var.controlplane_image
  worker_ami_id_override      = var.worker_ami_id
  resolve_worker_ami          = var.create_worker
}

module "controlplane" {
  source = "../modules/controlplane"

  name                                       = local.name
  bucket_name_prefix                         = var.bucket_name_prefix
  vpc_id                                     = module.controlplane_network.vpc_id
  public_subnet_ids                          = module.controlplane_network.public_subnet_ids
  private_subnet_ids                         = module.controlplane_network.private_subnet_ids
  public_url                                 = var.public_url
  api_origin                                 = var.api_origin
  deployment_mode                            = var.deployment_mode
  bootstrap_worker_group_name                = var.worker_group_name
  bootstrap_region_id                        = var.region_id
  clickhouse_url                             = var.clickhouse_url
  clickhouse_user                            = var.clickhouse_user
  clickhouse_password_secret_arn             = var.clickhouse_password_secret_arn
  clickhouse_password_kms_key_arns           = var.clickhouse_password_kms_key_arns
  additional_controlplane_security_group_ids = var.additional_controlplane_security_group_ids
  cloudfront_origin_domain_name              = var.cloudfront_origin_domain_name
  controlplane_image                         = module.release_artifacts.controlplane_image
  controlplane_image_repository_arn          = var.controlplane_image_repository_arn
  platform_store_uri                         = var.platform_store_uri
  platform_store_bucket_arn                  = var.platform_store_bucket_arn
  platform_store_kms_key_arn                 = var.platform_store_kms_key_arn
  controlplane_desired_count                 = var.controlplane_desired_count
  dispatcher_desired_count                   = var.dispatcher_desired_count
  controlplane_assign_public_ip              = var.controlplane_assign_public_ip
  create_controlplane_service                = var.create_controlplane_service
  controlplane_environment                   = var.controlplane_environment
  email_provider                             = var.email_provider
  email_from                                 = var.email_from
  smtp_addr                                  = var.smtp_addr
  smtp_username                              = var.smtp_username
  smtp_password_enabled                      = var.smtp_password_enabled
  redis_node_type                            = var.redis_node_type
  redis_node_count                           = var.redis_node_count
  certificate_arn                            = var.certificate_arn
  allow_insecure_http                        = var.allow_insecure_http
  enable_cloudfront                          = var.enable_cloudfront
  github_oauth_client_id                     = var.github_oauth_client_id
  database_instance_class                    = var.database_instance_class
  database_engine_version                    = var.database_engine_version
  database_allocated_storage_gb              = var.database_allocated_storage_gb
  database_backup_retention_days             = var.database_backup_retention_days
  database_performance_insights_enabled      = var.database_performance_insights_enabled
  database_deletion_protection               = var.database_deletion_protection
  database_skip_final_snapshot               = var.database_skip_final_snapshot
  controlplane_log_retention_days            = var.controlplane_log_retention_days
  kms_deletion_window_in_days                = var.kms_deletion_window_in_days
  secret_recovery_window_in_days             = var.secret_recovery_window_in_days
  cas_object_expiration_days                 = var.cas_object_expiration_days
  cas_noncurrent_version_expiration_days     = var.cas_noncurrent_version_expiration_days
  tags                                       = local.tags
}

module "worker_group" {
  for_each = var.create_worker ? toset(keys(local.worker_generation_specs)) : toset([])

  source = "../modules/worker"

  name                                            = local.worker_generations[each.key].provider_name
  worker_pool_name                                = each.key
  network_blocked_ipv4_cidrs                      = local.worker_generations[each.key].generation_inputs.supply.network.blocked_ipv4_cidrs
  network_link_pool                               = local.worker_generations[each.key].generation_inputs.supply.network.link_pool
  network_translation_pool                        = local.worker_generations[each.key].generation_inputs.supply.network.translation_pool
  network_resolver_ipv4                           = local.worker_generations[each.key].generation_inputs.supply.network.resolver_ipv4
  vpc_id                                          = module.execution_network.vpc_id
  subnet_ids                                      = module.execution_network.private_subnet_ids
  ami_id                                          = local.worker_generations[each.key].ami_id
  instance_type                                   = local.worker_generations[each.key].instance_type
  enable_nested_virtualization                    = local.worker_generations[each.key].enable_nested_virtualization
  enable_ssm                                      = local.worker_generations[each.key].generation_inputs.supply.enable_ssm
  sealed_provider_definition                      = local.worker_generations[each.key].sealed_provider_definition
  health_check_grace_period_seconds               = local.worker_generations[each.key].lifecycle.health_check_grace_period_seconds
  launch_lifecycle_heartbeat_timeout_seconds      = local.worker_generations[each.key].lifecycle.launch_lifecycle_heartbeat_timeout_seconds
  termination_lifecycle_heartbeat_timeout_seconds = local.worker_generations[each.key].lifecycle.termination_lifecycle_heartbeat_timeout_seconds
  termination_drain_timeout_seconds               = local.worker_generations[each.key].lifecycle.termination_drain_timeout_seconds
  lifecycle_heartbeat_interval_seconds            = local.worker_generations[each.key].lifecycle.lifecycle_heartbeat_interval_seconds
  min_size                                        = local.worker_generations[each.key].min_size
  max_size                                        = local.worker_generations[each.key].max_size
  root_volume_size_gb                             = local.worker_generations[each.key].root_volume_size_gb
  root_volume_iops                                = local.worker_generations[each.key].root_volume_iops
  root_volume_throughput                          = local.worker_generations[each.key].root_volume_throughput
  worker_disk_mib                                 = local.worker_generations[each.key].worker_disk_mib
  worker_disk_reserve_mib                         = local.worker_generations[each.key].worker_disk_reserve_mib
  vm_vcpus                                        = local.worker_generations[each.key].vm_vcpus
  vm_memory_mib                                   = local.worker_generations[each.key].vm_memory_mib
  vm_scratch_disk_mib                             = local.worker_generations[each.key].vm_scratch_disk_mib
  worker_capacity_vcpus                           = local.worker_generations[each.key].worker_capacity_vcpus
  worker_capacity_memory_mib                      = local.worker_generations[each.key].worker_capacity_memory_mib
  worker_execution_slots                          = local.worker_generations[each.key].worker_execution_slots
  substrate_cache_max_mib                         = local.worker_generations[each.key].substrate_cache_max_mib
  artifact_cache_max_mib                          = local.worker_generations[each.key].artifact_cache_max_mib
  worker_controlplane_url                         = local.worker_controlplane_url
  cas_uri                                         = module.controlplane.cas_uri
  cas_bucket_arn                                  = module.controlplane.cas_bucket_arn
  kms_key_arn                                     = module.controlplane.kms_key_arn
  platform_store_uri                              = local.worker_generations[each.key].generation_inputs.supply.platform_store.uri
  platform_store_bucket_arn                       = local.worker_generations[each.key].generation_inputs.supply.platform_store.bucket_arn
  platform_store_kms_key_arn                      = local.worker_generations[each.key].generation_inputs.supply.platform_store.kms_key_arn

  secret_arns = {
    checkpoint_encryption_key = module.controlplane.secret_arns.checkpoint_encryption_key
    worker_enrollment_token   = module.controlplane.worker_enrollment_secret_arn
  }

  tags = local.tags
}

resource "terraform_data" "quickstart_preconditions" {
  input = {
    controlplane_assign_public_ip = var.controlplane_assign_public_ip
    create_worker                 = var.create_worker
    cloudfront_origin             = var.cloudfront_origin_domain_name
    enable_cloudfront             = var.enable_cloudfront
    enable_nat_gateway            = var.enable_nat_gateway
    public_url                    = var.public_url
    worker_ami_id                 = module.release_artifacts.worker_ami_id
    worker_max_size               = var.worker_max_size
    worker_min_size               = var.worker_min_size
  }

  lifecycle {
    precondition {
      condition     = var.enable_cloudfront || var.public_url != null
      error_message = "public_url is required when enable_cloudfront is false."
    }

    precondition {
      condition     = !var.enable_cloudfront || (var.cloudfront_origin_domain_name != null && var.certificate_arn != null)
      error_message = "enable_cloudfront requires cloudfront_origin_domain_name and certificate_arn because CloudFront uses a TLS ALB origin."
    }

    precondition {
      condition     = var.controlplane_assign_public_ip || var.enable_nat_gateway
      error_message = "enable_nat_gateway must be true when controlplane_assign_public_ip is false because controlplane and migration tasks need outbound access."
    }

    precondition {
      condition     = !var.create_worker || var.enable_nat_gateway
      error_message = "enable_nat_gateway must be true when create_worker is true because workers run in private subnets."
    }

    precondition {
      condition = !var.create_worker || (
        var.worker_disk_mib != null &&
        coalesce(var.worker_disk_mib, 0) > var.worker_disk_reserve_mib &&
        coalesce(var.worker_capacity_vcpus, 0) > 0 &&
        coalesce(var.worker_capacity_memory_mib, 0) > 0 &&
        coalesce(var.worker_execution_slots, 0) > 0 &&
        local.worker_substrate_cache_mib > 0 && local.worker_artifact_cache_mib > 0 &&
        local.worker_guest_ephemeral_disk_mib >= var.worker_vm_scratch_disk_mib
      )
      error_message = "worker groups require configured CPU, memory, cache, disk, and execution-slot capacity."
    }

  }
}
