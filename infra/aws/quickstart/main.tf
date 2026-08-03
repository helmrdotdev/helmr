data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

locals {
  name                    = lower(var.name)
  worker_controlplane_url = module.controlplane.controlplane_url
  worker_ami_id           = coalesce(module.release_artifacts.worker_ami_id, "ami-unconfigured")
  boot_corpus_reserve_mib = 2048
  build_scratch_min_mib   = max(32768, coalesce(var.build_worker_vm_scratch_disk_mib, var.worker_vm_scratch_disk_mib)) + local.boot_corpus_reserve_mib
  build_worker_cpu_millis = coalesce(var.build_worker_capacity_vcpus, var.worker_capacity_vcpus, 0) * 1000
  build_worker_memory_mib = coalesce(var.build_worker_capacity_memory_mib, var.worker_capacity_memory_mib, 0)
  worker_pools = {
    run = {
      name         = "${local.name}-run"
      group_id     = var.worker_group_id
      roles        = ["run"]
      allows_run   = true
      allows_build = false
      min_size     = var.worker_min_size
      max_size     = var.worker_max_size
    }
    build = {
      name         = "${local.name}-build"
      group_id     = "${var.worker_group_id}-build"
      roles        = ["build"]
      allows_run   = false
      allows_build = true
      min_size     = var.build_worker_min_size
      max_size     = var.build_worker_max_size
    }
  }
  worker_groups = [for pool in values(local.worker_pools) : {
    id                      = pool.group_id
    name                    = one(pool.roles)
    description             = "${title(one(pool.roles))} workers"
    allows_run              = pool.allows_run
    allows_build            = pool.allows_build
    observation_ttl_seconds = var.worker_observation_ttl_seconds
    instance_capacity = !var.create_worker ? {
      milli_cpu         = 1, memory_bytes = 1, guest_ephemeral_disk_bytes = 1,
      build_cache_bytes = 0, artifact_cache_bytes = 0,
      vm_slots          = pool.allows_run ? 1 : 0, build_executors = pool.allows_build ? 1 : 0
      } : pool.allows_run ? {
      milli_cpu                  = coalesce(var.worker_capacity_vcpus, 0) * 1000
      memory_bytes               = coalesce(var.worker_capacity_memory_mib, 0) * 1048576
      guest_ephemeral_disk_bytes = local.run_worker_guest_ephemeral_disk_mib * 1048576
      build_cache_bytes          = local.run_worker_build_cache_mib * 1048576
      artifact_cache_bytes       = local.run_worker_artifact_cache_mib * 1048576
      vm_slots                   = coalesce(var.worker_execution_slots, 0)
      build_executors            = 0
      } : {
      milli_cpu                  = local.build_worker_cpu_millis
      memory_bytes               = local.build_worker_memory_mib * 1048576
      guest_ephemeral_disk_bytes = local.build_worker_guest_ephemeral_disk_mib * 1048576
      build_cache_bytes          = local.build_worker_build_cache_mib * 1048576
      artifact_cache_bytes       = local.build_worker_artifact_cache_mib * 1048576
      vm_slots                   = 0
      build_executors            = coalesce(var.build_worker_execution_slots, var.worker_execution_slots, 0)
    }
  }]
  run_worker_build_cache_mib            = coalesce(var.worker_substrate_cache_max_mib, 0)
  run_worker_artifact_cache_mib         = coalesce(var.worker_artifact_cache_max_mib, 0)
  run_worker_disk_reserve_mib           = var.worker_disk_reserve_mib
  run_worker_shared_disk_mib            = coalesce(var.worker_disk_mib, 0) - local.run_worker_disk_reserve_mib - local.run_worker_build_cache_mib - local.run_worker_artifact_cache_mib
  run_worker_guest_ephemeral_disk_mib   = local.run_worker_shared_disk_mib
  build_worker_build_cache_mib          = coalesce(var.build_worker_substrate_cache_max_mib, var.worker_substrate_cache_max_mib, 0)
  build_worker_artifact_cache_mib       = coalesce(var.build_worker_artifact_cache_max_mib, var.worker_artifact_cache_max_mib, 0)
  build_worker_disk_reserve_mib         = coalesce(var.build_worker_disk_reserve_mib, var.worker_disk_reserve_mib)
  build_worker_shared_disk_mib          = coalesce(var.build_worker_disk_mib, var.worker_disk_mib, 0) - local.build_worker_disk_reserve_mib - local.build_worker_build_cache_mib - local.build_worker_artifact_cache_mib
  build_worker_scratch_mib              = local.build_worker_shared_disk_mib
  build_worker_guest_ephemeral_disk_mib = local.build_worker_scratch_mib - local.boot_corpus_reserve_mib
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
  deployment_mode                            = var.deployment_mode
  worker_groups                              = local.worker_groups
  image_cache_worker_role_arns               = [for pool in values(local.worker_pools) : "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role/${pool.name}-worker" if pool.allows_build]
  region_id                                  = var.region_id
  default_region_id                          = var.default_region_id
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
  build_policy_digest                        = var.build_policy_digest
  controlplane_desired_count                 = var.controlplane_desired_count
  dispatcher_desired_count                   = var.dispatcher_desired_count
  controlplane_assign_public_ip              = var.controlplane_assign_public_ip
  controlplane_health_check_path             = var.controlplane_health_check_path
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
  for_each = var.create_worker ? local.worker_pools : {}

  source = "../modules/worker"

  name                                       = each.value.name
  worker_group_id                            = each.value.group_id
  worker_roles                               = each.value.roles
  network_blocked_ipv4_cidrs                 = var.worker_network_blocked_ipv4_cidrs
  network_link_pool                          = var.worker_network_link_pool
  network_translation_pool                   = var.worker_network_translation_pool
  network_resolver_ipv4                      = var.worker_network_resolver_ipv4
  vpc_id                                     = module.execution_network.vpc_id
  subnet_ids                                 = module.execution_network.private_subnet_ids
  ami_id                                     = module.release_artifacts.worker_ami_id
  instance_type                              = each.key == "build" ? coalesce(var.build_worker_instance_type, var.worker_instance_type) : var.worker_instance_type
  enable_nested_virtualization               = each.key == "build" && var.build_worker_enable_nested_virtualization != null ? var.build_worker_enable_nested_virtualization : var.worker_enable_nested_virtualization
  enable_ssm                                 = var.worker_enable_ssm
  launch_lifecycle_heartbeat_timeout_seconds = var.worker_launch_timeout_seconds
  min_size                                   = each.value.min_size
  max_size                                   = each.value.max_size
  root_volume_size_gb                        = each.key == "build" ? coalesce(var.build_worker_root_volume_size_gb, var.worker_root_volume_size_gb) : var.worker_root_volume_size_gb
  root_volume_iops                           = each.key == "build" ? coalesce(var.build_worker_root_volume_iops, var.worker_root_volume_iops) : var.worker_root_volume_iops
  root_volume_throughput                     = each.key == "build" ? coalesce(var.build_worker_root_volume_throughput, var.worker_root_volume_throughput) : var.worker_root_volume_throughput
  worker_disk_mib                            = each.key == "build" && var.build_worker_disk_mib != null ? var.build_worker_disk_mib : var.worker_disk_mib
  worker_disk_reserve_mib                    = each.key == "build" ? local.build_worker_disk_reserve_mib : local.run_worker_disk_reserve_mib
  vm_vcpus                                   = each.key == "build" && var.build_worker_vm_vcpus != null ? var.build_worker_vm_vcpus : var.worker_vm_vcpus
  vm_memory_mib                              = each.key == "build" && var.build_worker_vm_memory_mib != null ? var.build_worker_vm_memory_mib : var.worker_vm_memory_mib
  vm_scratch_disk_mib                        = each.key == "build" && var.build_worker_vm_scratch_disk_mib != null ? var.build_worker_vm_scratch_disk_mib : var.worker_vm_scratch_disk_mib
  worker_capacity_vcpus                      = each.key == "build" && var.build_worker_capacity_vcpus != null ? var.build_worker_capacity_vcpus : var.worker_capacity_vcpus
  worker_capacity_memory_mib                 = each.key == "build" && var.build_worker_capacity_memory_mib != null ? var.build_worker_capacity_memory_mib : var.worker_capacity_memory_mib
  worker_execution_slots                     = each.key == "build" && var.build_worker_execution_slots != null ? var.build_worker_execution_slots : var.worker_execution_slots
  substrate_cache_max_mib                    = each.key == "build" && var.build_worker_substrate_cache_max_mib != null ? var.build_worker_substrate_cache_max_mib : var.worker_substrate_cache_max_mib
  artifact_cache_max_mib                     = each.key == "build" && var.build_worker_artifact_cache_max_mib != null ? var.build_worker_artifact_cache_max_mib : var.worker_artifact_cache_max_mib
  build_cache_mib                            = each.key == "build" ? local.build_worker_build_cache_mib + local.build_worker_artifact_cache_mib : null
  build_scratch_mib                          = each.key == "build" ? local.build_worker_scratch_mib : null
  worker_controlplane_url                    = local.worker_controlplane_url
  cas_uri                                    = module.controlplane.cas_uri
  cas_bucket_arn                             = module.controlplane.cas_bucket_arn
  kms_key_arn                                = module.controlplane.kms_key_arn
  platform_store_uri                         = var.platform_store_uri
  platform_store_bucket_arn                  = var.platform_store_bucket_arn
  platform_store_kms_key_arn                 = var.platform_store_kms_key_arn
  build_policy_digest                        = each.key == "build" ? var.build_policy_digest : null
  image_cache_registry_authority             = module.controlplane.image_cache_registry_authority
  image_cache_repository_prefix              = module.controlplane.image_cache_repository_prefix
  image_cache_role_arn                       = module.controlplane.image_cache_role_arn
  image_cache_repository_arn_prefix          = module.controlplane.image_cache_repository_arn_prefix

  secret_arns = {
    checkpoint_encryption_key = module.controlplane.secret_arns.checkpoint_encryption_key
    worker_enrollment         = module.controlplane.worker_enrollment_secret_arns[each.value.group_id]
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
        coalesce(var.worker_disk_mib, 0) > local.run_worker_disk_reserve_mib &&
        coalesce(var.build_worker_disk_mib, var.worker_disk_mib, 0) > local.build_worker_disk_reserve_mib &&
        coalesce(var.worker_capacity_vcpus, 0) > 0 &&
        coalesce(var.worker_capacity_memory_mib, 0) > 0 &&
        coalesce(var.worker_execution_slots, 0) > 0 &&
        local.run_worker_build_cache_mib > 0 && local.run_worker_artifact_cache_mib > 0 &&
        local.run_worker_guest_ephemeral_disk_mib >= var.worker_vm_scratch_disk_mib &&
        local.build_worker_cpu_millis >= 2000 &&
        local.build_worker_memory_mib >= 2048 &&
        coalesce(var.build_worker_execution_slots, var.worker_execution_slots, 0) == 1 &&
        local.build_worker_build_cache_mib > 0 && local.build_worker_artifact_cache_mib > 0 &&
        local.build_worker_guest_ephemeral_disk_mib >= coalesce(var.build_worker_vm_scratch_disk_mib, var.worker_vm_scratch_disk_mib) &&
        local.build_worker_scratch_mib >= local.build_scratch_min_mib
      )
      error_message = "worker groups require configured CPU, memory, cache, disk partitions, and concurrency; build capacity must fit one fixed build guest after the service reserve and expose exactly one build executor."
    }

  }
}
