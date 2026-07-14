data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

locals {
  name                     = lower(var.name)
  public_url_host          = var.public_url == null ? null : regex("^https?://([^/:]+)", var.public_url)[0]
  worker_control_dns_name  = var.enable_cloudfront ? var.cloudfront_origin_domain_name : local.public_url_host
  private_control_dns_name = var.create_worker ? local.worker_control_dns_name : null
  worker_control_url       = module.control.private_control_url
  worker_ami_id            = coalesce(module.release_artifacts.worker_ami_id, "ami-unconfigured")
  worker_allowed_ami_ids   = distinct(compact(concat([local.worker_ami_id], var.worker_allowed_ami_ids)))
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
    id                   = pool.group_id
    name                 = one(pool.roles)
    description          = "${title(one(pool.roles))} workers"
    region               = var.aws_region
    account_id           = data.aws_caller_identity.current.account_id
    autoscaling_group    = "${pool.name}-worker"
    instance_profile_arn = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:instance-profile/${pool.name}-worker"
    launch_ami_id        = local.worker_ami_id
    ami_ids              = local.worker_allowed_ami_ids
    allows_run           = pool.allows_run
    allows_build         = pool.allows_build
    instance_capacity = !var.create_worker ? {
      milli_cpu         = 1, memory_bytes = 1, workload_disk_bytes = 1, scratch_bytes = 1,
      build_cache_bytes = 0, artifact_cache_bytes = 0,
      vm_slots          = pool.allows_run ? 1 : 0, build_executors = pool.allows_build ? 1 : 0
      } : pool.allows_run ? {
      milli_cpu            = coalesce(var.worker_capacity_vcpus, 0) * 1000
      memory_bytes         = coalesce(var.worker_capacity_memory_mib, 0) * 1048576
      workload_disk_bytes  = local.run_worker_workload_disk_mib * 1048576
      scratch_bytes        = local.run_worker_scratch_mib * 1048576
      build_cache_bytes    = local.run_worker_build_cache_mib * 1048576
      artifact_cache_bytes = local.run_worker_artifact_cache_mib * 1048576
      vm_slots             = coalesce(var.worker_execution_slots, 0)
      build_executors      = 0
      } : {
      milli_cpu            = coalesce(var.build_worker_capacity_vcpus, var.worker_capacity_vcpus, 0) * 1000
      memory_bytes         = coalesce(var.build_worker_capacity_memory_mib, var.worker_capacity_memory_mib, 0) * 1048576
      workload_disk_bytes  = local.build_worker_workload_disk_mib * 1048576
      scratch_bytes        = local.build_worker_scratch_mib * 1048576
      build_cache_bytes    = local.build_worker_build_cache_mib * 1048576
      artifact_cache_bytes = local.build_worker_artifact_cache_mib * 1048576
      vm_slots             = 0
      build_executors      = coalesce(var.build_worker_execution_slots, var.worker_execution_slots, 0)
    }
  }]
  run_worker_build_cache_mib      = coalesce(var.worker_substrate_cache_max_mib, 0)
  run_worker_artifact_cache_mib   = coalesce(var.worker_artifact_cache_max_mib, 0)
  run_worker_disk_reserve_mib     = var.worker_disk_reserve_mib
  run_worker_shared_disk_mib      = coalesce(var.worker_disk_mib, 0) - local.run_worker_disk_reserve_mib - local.run_worker_build_cache_mib - local.run_worker_artifact_cache_mib
  run_worker_workload_disk_mib    = floor(local.run_worker_shared_disk_mib / 2)
  run_worker_scratch_mib          = ceil(local.run_worker_shared_disk_mib / 2)
  build_worker_build_cache_mib    = coalesce(var.build_worker_substrate_cache_max_mib, var.worker_substrate_cache_max_mib, 0)
  build_worker_artifact_cache_mib = coalesce(var.build_worker_artifact_cache_max_mib, var.worker_artifact_cache_max_mib, 0)
  build_worker_disk_reserve_mib   = coalesce(var.build_worker_disk_reserve_mib, var.worker_disk_reserve_mib)
  build_worker_shared_disk_mib    = coalesce(var.build_worker_disk_mib, var.worker_disk_mib, 0) - local.build_worker_disk_reserve_mib - local.build_worker_build_cache_mib - local.build_worker_artifact_cache_mib
  build_worker_workload_disk_mib  = floor(local.build_worker_shared_disk_mib / 2)
  build_worker_scratch_mib        = ceil(local.build_worker_shared_disk_mib / 2)
  worker_fleets = var.create_worker ? [
    {
      group_id           = local.worker_pools.run.group_id
      autoscaling_group  = "${local.worker_pools.run.name}-worker"
      role               = "run"
      compatibility_keys = [local.worker_pools.run.group_id]
      instance_capacity = {
        milli_cpu            = coalesce(var.worker_capacity_vcpus, 0) * 1000
        memory_bytes         = coalesce(var.worker_capacity_memory_mib, 0) * 1048576
        workload_disk_bytes  = local.run_worker_workload_disk_mib * 1048576
        scratch_bytes        = local.run_worker_scratch_mib * 1048576
        build_cache_bytes    = local.run_worker_build_cache_mib * 1048576
        artifact_cache_bytes = local.run_worker_artifact_cache_mib * 1048576
        vm_slots             = coalesce(var.worker_execution_slots, 0)
        build_executors      = 0
      }
      queued_run_scratch_bytes     = var.worker_vm_scratch_disk_mib * 1048576
      min_workers                  = var.worker_min_size
      warm_workers                 = var.worker_fleet_controller.run_warm_workers
      max_workers                  = coalesce(var.worker_fleet_controller.run_max_workers, var.worker_max_size)
      max_scale_out_per_cycle      = var.worker_fleet_controller.max_scale_out_per_cycle
      max_pending_workers          = var.worker_fleet_controller.max_pending_workers
      max_packing_items            = var.worker_fleet_controller.max_packing_items
      controller_interval_seconds  = var.worker_fleet_controller.controller_interval_seconds
      scale_out_cooldown_seconds   = var.worker_fleet_controller.scale_out_cooldown_seconds
      scale_in_cooldown_seconds    = var.worker_fleet_controller.scale_in_cooldown_seconds
      scale_in_hysteresis_seconds  = var.worker_fleet_controller.scale_in_hysteresis_seconds
      stale_worker_timeout_seconds = var.worker_fleet_controller.stale_worker_timeout_seconds
      readiness_timeout_seconds    = var.worker_fleet_controller.readiness_timeout_seconds
      drain_timeout_seconds        = var.worker_fleet_controller.drain_timeout_seconds
      emergency_stop               = var.worker_fleet_controller.emergency_stop
      metric_interval_seconds      = var.worker_fleet_controller.metric_interval_seconds
    },
    {
      group_id           = local.worker_pools.build.group_id
      autoscaling_group  = "${local.worker_pools.build.name}-worker"
      role               = "build"
      compatibility_keys = [local.worker_pools.build.group_id]
      instance_capacity = {
        milli_cpu            = coalesce(var.build_worker_capacity_vcpus, var.worker_capacity_vcpus, 0) * 1000
        memory_bytes         = coalesce(var.build_worker_capacity_memory_mib, var.worker_capacity_memory_mib, 0) * 1048576
        workload_disk_bytes  = local.build_worker_workload_disk_mib * 1048576
        scratch_bytes        = local.build_worker_scratch_mib * 1048576
        build_cache_bytes    = local.build_worker_build_cache_mib * 1048576
        artifact_cache_bytes = local.build_worker_artifact_cache_mib * 1048576
        vm_slots             = 0
        build_executors      = coalesce(var.build_worker_execution_slots, var.worker_execution_slots, 0)
      }
      queued_run_scratch_bytes     = 0
      min_workers                  = var.build_worker_min_size
      warm_workers                 = var.worker_fleet_controller.build_warm_workers
      max_workers                  = coalesce(var.worker_fleet_controller.build_max_workers, var.build_worker_max_size)
      max_scale_out_per_cycle      = var.worker_fleet_controller.max_scale_out_per_cycle
      max_pending_workers          = var.worker_fleet_controller.max_pending_workers
      max_packing_items            = var.worker_fleet_controller.max_packing_items
      controller_interval_seconds  = var.worker_fleet_controller.controller_interval_seconds
      scale_out_cooldown_seconds   = var.worker_fleet_controller.scale_out_cooldown_seconds
      scale_in_cooldown_seconds    = var.worker_fleet_controller.scale_in_cooldown_seconds
      scale_in_hysteresis_seconds  = var.worker_fleet_controller.scale_in_hysteresis_seconds
      stale_worker_timeout_seconds = var.worker_fleet_controller.stale_worker_timeout_seconds
      readiness_timeout_seconds    = var.worker_fleet_controller.readiness_timeout_seconds
      drain_timeout_seconds        = var.worker_fleet_controller.drain_timeout_seconds
      emergency_stop               = var.worker_fleet_controller.emergency_stop
      metric_interval_seconds      = var.worker_fleet_controller.metric_interval_seconds
    }
  ] : []

  tags = merge({
    Project     = "helmr"
    Application = "helmr"
    Environment = var.environment
    Example     = "quickstart"
    ManagedBy   = "terraform"
  }, var.tags)
}

module "network" {
  source = "../modules/network"

  name                    = local.name
  vpc_cidr                = var.vpc_cidr
  availability_zone_count = var.availability_zone_count
  enable_nat_gateway      = var.enable_nat_gateway
  tags                    = local.tags
}

module "release_artifacts" {
  source = "../modules/release-artifacts"

  helmr_version          = var.helmr_version
  aws_region             = var.aws_region
  manifest_base_url      = var.release_artifacts_manifest_base_url
  manifest_url           = var.release_artifacts_manifest_url
  control_image_override = var.control_image
  worker_ami_id_override = var.worker_ami_id
  resolve_worker_ami     = var.create_worker
}

module "control" {
  source = "../modules/control"

  name                                   = local.name
  bucket_name_prefix                     = var.bucket_name_prefix
  vpc_id                                 = module.network.vpc_id
  public_subnet_ids                      = module.network.public_subnet_ids
  private_subnet_ids                     = module.network.private_subnet_ids
  public_url                             = var.public_url
  deployment_mode                        = var.deployment_mode
  worker_group_id                        = var.worker_group_id
  worker_groups                          = local.worker_groups
  worker_fleets                          = local.worker_fleets
  region_id                              = var.region_id
  default_region_id                      = var.default_region_id
  clickhouse_url                         = var.clickhouse_url
  clickhouse_user                        = var.clickhouse_user
  clickhouse_password_secret_arn         = var.clickhouse_password_secret_arn
  clickhouse_password_kms_key_arns       = var.clickhouse_password_kms_key_arns
  additional_control_security_group_ids  = var.additional_control_security_group_ids
  cloudfront_origin_domain_name          = var.cloudfront_origin_domain_name
  control_image                          = module.release_artifacts.control_image
  control_desired_count                  = var.control_desired_count
  dispatcher_desired_count               = var.dispatcher_desired_count
  control_assign_public_ip               = var.control_assign_public_ip
  control_health_check_path              = var.control_health_check_path
  create_control_service                 = var.create_control_service
  control_environment                    = var.control_environment
  email_provider                         = var.email_provider
  email_from                             = var.email_from
  smtp_addr                              = var.smtp_addr
  smtp_username                          = var.smtp_username
  smtp_password_enabled                  = var.smtp_password_enabled
  redis_node_type                        = var.redis_node_type
  redis_node_count                       = var.redis_node_count
  certificate_arn                        = var.certificate_arn
  allow_insecure_http                    = var.allow_insecure_http
  enable_cloudfront                      = var.enable_cloudfront
  private_control_dns_name               = local.private_control_dns_name
  github_oauth_client_id                 = var.github_oauth_client_id
  secret_encryption_key_old_arn          = var.secret_encryption_key_old_arn
  secret_encryption_key_old_kms_key_arns = var.secret_encryption_key_old_kms_key_arns
  database_instance_class                = var.database_instance_class
  database_engine_version                = var.database_engine_version
  database_allocated_storage_gb          = var.database_allocated_storage_gb
  database_backup_retention_days         = var.database_backup_retention_days
  database_performance_insights_enabled  = var.database_performance_insights_enabled
  database_deletion_protection           = var.database_deletion_protection
  database_skip_final_snapshot           = var.database_skip_final_snapshot
  control_log_retention_days             = var.control_log_retention_days
  kms_deletion_window_in_days            = var.kms_deletion_window_in_days
  secret_recovery_window_in_days         = var.secret_recovery_window_in_days
  cas_object_expiration_days             = var.cas_object_expiration_days
  cas_noncurrent_version_expiration_days = var.cas_noncurrent_version_expiration_days
  tags                                   = local.tags
}

module "worker_group" {
  for_each = var.create_worker ? local.worker_pools : {}

  source = "../modules/worker"

  name                                       = each.value.name
  worker_group_id                            = each.value.group_id
  worker_roles                               = each.value.roles
  vpc_id                                     = module.network.vpc_id
  subnet_ids                                 = module.network.private_subnet_ids
  ami_id                                     = module.release_artifacts.worker_ami_id
  instance_type                              = each.key == "build" ? coalesce(var.build_worker_instance_type, var.worker_instance_type) : var.worker_instance_type
  enable_nested_virtualization               = each.key == "build" && var.build_worker_enable_nested_virtualization != null ? var.build_worker_enable_nested_virtualization : var.worker_enable_nested_virtualization
  enable_ssm                                 = var.worker_enable_ssm
  launch_lifecycle_heartbeat_timeout_seconds = var.worker_fleet_controller.readiness_timeout_seconds
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
  worker_control_url                         = local.worker_control_url
  cas_uri                                    = module.control.cas_uri
  cas_bucket_arn                             = module.control.cas_bucket_arn
  kms_key_arn                                = module.control.kms_key_arn

  secret_arns = {
    checkpoint_encryption_key = module.control.secret_arns.checkpoint_encryption_key
  }

  tags = local.tags
}

resource "terraform_data" "quickstart_preconditions" {
  input = {
    control_assign_public_ip = var.control_assign_public_ip
    create_worker            = var.create_worker
    cloudfront_origin        = var.cloudfront_origin_domain_name
    enable_cloudfront        = var.enable_cloudfront
    enable_nat_gateway       = var.enable_nat_gateway
    public_url               = var.public_url
    worker_ami_id            = module.release_artifacts.worker_ami_id
    worker_max_size          = var.worker_max_size
    worker_min_size          = var.worker_min_size
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
      condition     = var.control_assign_public_ip || var.enable_nat_gateway
      error_message = "enable_nat_gateway must be true when control_assign_public_ip is false because control and migration tasks need outbound access."
    }

    precondition {
      condition     = !var.create_worker || var.enable_nat_gateway
      error_message = "enable_nat_gateway must be true when create_worker is true because workers run in private subnets."
    }

    precondition {
      condition     = !var.create_worker || (try(trimspace(local.worker_control_dns_name) != "", false) && try(trimspace(var.certificate_arn) != "", false))
      error_message = "create_worker requires certificate_arn and a private worker control DNS name derived from public_url or cloudfront_origin_domain_name."
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
        local.run_worker_workload_disk_mib >= var.worker_vm_scratch_disk_mib &&
        local.run_worker_scratch_mib >= var.worker_vm_scratch_disk_mib &&
        coalesce(var.build_worker_capacity_vcpus, var.worker_capacity_vcpus, 0) > 0 &&
        coalesce(var.build_worker_capacity_memory_mib, var.worker_capacity_memory_mib, 0) > 0 &&
        coalesce(var.build_worker_execution_slots, var.worker_execution_slots, 0) > 0 &&
        local.build_worker_build_cache_mib > 0 && local.build_worker_artifact_cache_mib > 0 &&
        local.build_worker_workload_disk_mib >= coalesce(var.build_worker_vm_scratch_disk_mib, var.worker_vm_scratch_disk_mib) &&
        local.build_worker_scratch_mib >= coalesce(var.build_worker_vm_scratch_disk_mib, var.worker_vm_scratch_disk_mib)
      )
      error_message = "worker groups require explicit certified CPU, memory, cache, disk-partition, and execution-slot capacity; one VM workload and scratch shape must fit each host partition."
    }

    precondition {
      condition = !var.create_worker || (
        coalesce(var.worker_fleet_controller.run_max_workers, var.worker_max_size) <= var.worker_max_size &&
        coalesce(var.worker_fleet_controller.build_max_workers, var.build_worker_max_size) <= var.build_worker_max_size
      )
      error_message = "fleet policy max_workers cannot exceed its Auto Scaling group max_size."
    }
  }
}
