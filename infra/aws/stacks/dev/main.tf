data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

locals {
  public_url_host              = regex("^https?://([^/:]+)", var.public_url)[0]
  worker_control_dns_name      = var.enable_cloudfront ? var.cloudfront_origin_domain_name : local.public_url_host
  private_control_dns_name     = var.create_worker ? local.worker_control_dns_name : null
  worker_control_url           = module.control.private_control_url
  external_clickhouse_url      = var.clickhouse_url == null ? null : trimspace(var.clickhouse_url)
  managed_clickhouse_url       = one(module.clickhouse[*].clickhouse_url)
  managed_clickhouse_user      = one(module.clickhouse[*].clickhouse_user)
  managed_clickhouse_secret    = one(module.clickhouse[*].clickhouse_password_secret_arn)
  managed_clickhouse_kms_key   = one(module.clickhouse[*].clickhouse_password_kms_key_id)
  managed_clickhouse_client_sg = one(module.clickhouse[*].client_security_group_id)
  clickhouse_url               = var.create_clickhouse_cloud ? local.managed_clickhouse_url : local.external_clickhouse_url
  clickhouse_user              = var.create_clickhouse_cloud ? local.managed_clickhouse_user : var.clickhouse_user
  clickhouse_password_secret   = var.create_clickhouse_cloud ? local.managed_clickhouse_secret : var.clickhouse_password_secret_arn
  clickhouse_kms_key_arns      = var.create_clickhouse_cloud ? compact([local.managed_clickhouse_kms_key]) : var.clickhouse_password_kms_key_arns
  control_security_group_ids   = concat(var.additional_control_security_group_ids, var.create_clickhouse_cloud ? compact([local.managed_clickhouse_client_sg]) : [])
  run_worker_group_id          = var.worker_group_id
  build_worker_group_id        = "${var.worker_group_id}-build"
  run_worker_name              = "${lower(var.name)}-run"
  build_worker_name            = "${lower(var.name)}-build"
  worker_ami_id                = coalesce(var.worker_ami_id, "ami-unconfigured")
  worker_allowed_ami_ids       = distinct(compact(concat([local.worker_ami_id], var.worker_allowed_ami_ids)))
  worker_groups = [
    {
      id                   = local.run_worker_group_id
      name                 = "run"
      description          = "Run workers"
      region               = var.aws_region
      account_id           = data.aws_caller_identity.current.account_id
      autoscaling_group    = "${local.run_worker_name}-worker"
      instance_profile_arn = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:instance-profile/${local.run_worker_name}-worker"
      launch_ami_id        = local.worker_ami_id
      ami_ids              = local.worker_allowed_ami_ids
      allows_run           = true
      allows_build         = false
      instance_capacity = var.create_worker ? {
        milli_cpu            = coalesce(var.worker_capacity_vcpus, 0) * 1000
        memory_bytes         = coalesce(var.worker_capacity_memory_mib, 0) * 1048576
        workload_disk_bytes  = local.run_worker_workload_disk_mib * 1048576
        scratch_bytes        = local.run_worker_scratch_mib * 1048576
        build_cache_bytes    = local.run_worker_build_cache_mib * 1048576
        artifact_cache_bytes = local.run_worker_artifact_cache_mib * 1048576
        vm_slots             = coalesce(var.worker_execution_slots, 0)
        build_executors      = 0
        } : {
        milli_cpu         = 1, memory_bytes = 1, workload_disk_bytes = 1, scratch_bytes = 1,
        build_cache_bytes = 0, artifact_cache_bytes = 0, vm_slots = 1, build_executors = 0
      }
    },
    {
      id                   = local.build_worker_group_id
      name                 = "build"
      description          = "Build workers"
      region               = var.aws_region
      account_id           = data.aws_caller_identity.current.account_id
      autoscaling_group    = "${local.build_worker_name}-worker"
      instance_profile_arn = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:instance-profile/${local.build_worker_name}-worker"
      launch_ami_id        = local.worker_ami_id
      ami_ids              = local.worker_allowed_ami_ids
      allows_run           = false
      allows_build         = true
      instance_capacity = var.create_worker ? {
        milli_cpu            = coalesce(var.build_worker_capacity_vcpus, var.worker_capacity_vcpus, 0) * 1000
        memory_bytes         = coalesce(var.build_worker_capacity_memory_mib, var.worker_capacity_memory_mib, 0) * 1048576
        workload_disk_bytes  = local.build_worker_workload_disk_mib * 1048576
        scratch_bytes        = local.build_worker_scratch_mib * 1048576
        build_cache_bytes    = local.build_worker_build_cache_mib * 1048576
        artifact_cache_bytes = local.build_worker_artifact_cache_mib * 1048576
        vm_slots             = 0
        build_executors      = coalesce(var.build_worker_execution_slots, var.worker_execution_slots, 0)
        } : {
        milli_cpu         = 1, memory_bytes = 1, workload_disk_bytes = 1, scratch_bytes = 1,
        build_cache_bytes = 0, artifact_cache_bytes = 0, vm_slots = 0, build_executors = 1
      }
    }
  ]
  run_worker_build_cache_mib      = var.worker_substrate_cache_max_mib
  run_worker_artifact_cache_mib   = var.worker_artifact_cache_max_mib
  run_worker_disk_reserve_mib     = var.worker_disk_reserve_mib
  run_worker_shared_disk_mib      = coalesce(var.worker_disk_mib, 0) - local.run_worker_disk_reserve_mib - local.run_worker_build_cache_mib - local.run_worker_artifact_cache_mib
  run_worker_workload_disk_mib    = floor(local.run_worker_shared_disk_mib / 2)
  run_worker_scratch_mib          = ceil(local.run_worker_shared_disk_mib / 2)
  build_worker_build_cache_mib    = coalesce(var.build_worker_substrate_cache_max_mib, var.worker_substrate_cache_max_mib)
  build_worker_artifact_cache_mib = coalesce(var.build_worker_artifact_cache_max_mib, var.worker_artifact_cache_max_mib)
  build_worker_disk_reserve_mib   = coalesce(var.build_worker_disk_reserve_mib, var.worker_disk_reserve_mib)
  build_worker_shared_disk_mib    = coalesce(var.build_worker_disk_mib, var.worker_disk_mib, 0) - local.build_worker_disk_reserve_mib - local.build_worker_build_cache_mib - local.build_worker_artifact_cache_mib
  build_worker_workload_disk_mib  = floor(local.build_worker_shared_disk_mib / 2)
  build_worker_scratch_mib        = ceil(local.build_worker_shared_disk_mib / 2)
  worker_fleets = var.create_worker ? [
    {
      group_id           = local.run_worker_group_id
      autoscaling_group  = "${local.run_worker_name}-worker"
      role               = "run"
      compatibility_keys = [local.run_worker_group_id]
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
      group_id           = local.build_worker_group_id
      autoscaling_group  = "${local.build_worker_name}-worker"
      role               = "build"
      compatibility_keys = [local.build_worker_group_id]
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

  tags = {
    Project     = "helmr"
    Environment = "dev"
    ManagedBy   = "terraform"
    Stack       = lower(var.name)
  }
}

module "network" {
  source = "../../modules/network"

  name               = var.name
  enable_nat_gateway = var.enable_nat_gateway
  tags               = local.tags
}

module "clickhouse" {
  count = var.create_clickhouse_cloud ? 1 : 0

  source = "../../modules/clickhouse-cloud"

  name                             = var.name
  service_name                     = var.clickhouse_cloud_service_name
  clickhouse_region                = var.clickhouse_cloud_region
  secret_kms_key_id                = var.clickhouse_secret_kms_key_id
  vpc_id                           = module.network.vpc_id
  subnet_ids                       = module.network.private_subnet_ids
  min_replica_memory_gb            = var.clickhouse_min_replica_memory_gb
  max_replica_memory_gb            = var.clickhouse_max_replica_memory_gb
  idle_scaling                     = var.clickhouse_idle_scaling
  idle_timeout_minutes             = var.clickhouse_idle_timeout_minutes
  backup_retention_period_in_hours = var.clickhouse_backup_retention_period_in_hours
  secret_recovery_window_in_days   = var.secret_recovery_window_in_days
  tags                             = local.tags
}

module "control" {
  source = "../../modules/control"

  name                                       = var.name
  vpc_id                                     = module.network.vpc_id
  public_subnet_ids                          = module.network.public_subnet_ids
  private_subnet_ids                         = module.network.private_subnet_ids
  public_url                                 = var.public_url
  deployment_mode                            = var.deployment_mode
  worker_group_id                            = var.worker_group_id
  worker_groups                              = local.worker_groups
  worker_fleets                              = local.worker_fleets
  region_id                                  = var.region_id
  default_region_id                          = var.default_region_id
  clickhouse_url                             = local.clickhouse_url
  clickhouse_user                            = local.clickhouse_user
  clickhouse_password_secret_arn             = local.clickhouse_password_secret
  clickhouse_password_kms_key_arns           = local.clickhouse_kms_key_arns
  additional_control_security_group_ids      = local.control_security_group_ids
  cloudfront_origin_domain_name              = var.cloudfront_origin_domain_name
  control_image                              = var.control_image
  create_control_repository                  = true
  create_control_service                     = var.create_control_service
  control_desired_count                      = var.control_desired_count
  control_environment                        = var.control_environment
  dispatcher_desired_count                   = var.dispatcher_desired_count
  dispatcher_environment                     = var.dispatcher_environment
  control_assign_public_ip                   = var.control_assign_public_ip
  control_health_check_path                  = var.control_health_check_path
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
  private_control_dns_name                   = local.private_control_dns_name
  github_oauth_client_id                     = var.github_oauth_client_id
  secret_encryption_key_old_arn              = var.secret_encryption_key_old_arn
  secret_encryption_key_old_kms_key_arns     = var.secret_encryption_key_old_kms_key_arns
  database_backup_retention_days             = var.database_backup_retention_days
  database_engine_version                    = var.database_engine_version
  database_deletion_protection               = var.database_deletion_protection
  database_skip_final_snapshot               = var.database_skip_final_snapshot
  control_repository_force_delete            = var.control_repository_force_delete
  control_ecr_max_images                     = var.control_ecr_max_images
  control_ecr_untagged_image_expiration_days = var.control_ecr_untagged_image_expiration_days
  control_log_retention_days                 = var.control_log_retention_days
  kms_deletion_window_in_days                = var.kms_deletion_window_in_days
  secret_recovery_window_in_days             = var.secret_recovery_window_in_days
  cas_object_expiration_days                 = var.cas_object_expiration_days
  cas_noncurrent_version_expiration_days     = var.cas_noncurrent_version_expiration_days
  tags                                       = local.tags
}

module "run_worker" {
  count = var.create_worker ? 1 : 0

  source = "../../modules/worker"

  name                                       = local.run_worker_name
  worker_group_id                            = local.run_worker_group_id
  worker_roles                               = ["run"]
  vpc_id                                     = module.network.vpc_id
  subnet_ids                                 = module.network.private_subnet_ids
  ami_id                                     = var.worker_ami_id
  instance_type                              = var.worker_instance_type
  enable_nested_virtualization               = var.worker_enable_nested_virtualization
  enable_ssm                                 = var.worker_enable_ssm
  launch_lifecycle_heartbeat_timeout_seconds = var.worker_fleet_controller.readiness_timeout_seconds
  min_size                                   = var.worker_min_size
  max_size                                   = var.worker_max_size
  root_volume_size_gb                        = var.worker_root_volume_size_gb
  root_volume_iops                           = var.worker_root_volume_iops
  root_volume_throughput                     = var.worker_root_volume_throughput
  worker_disk_mib                            = var.worker_disk_mib
  worker_disk_reserve_mib                    = local.run_worker_disk_reserve_mib
  vm_vcpus                                   = var.worker_vm_vcpus
  vm_memory_mib                              = var.worker_vm_memory_mib
  vm_scratch_disk_mib                        = var.worker_vm_scratch_disk_mib
  worker_capacity_vcpus                      = var.worker_capacity_vcpus
  worker_capacity_memory_mib                 = var.worker_capacity_memory_mib
  worker_execution_slots                     = var.worker_execution_slots
  substrate_cache_max_mib                    = var.worker_substrate_cache_max_mib
  artifact_cache_max_mib                     = var.worker_artifact_cache_max_mib
  worker_environment                         = var.worker_environment
  buildkit_slirp_cidr                        = var.worker_buildkit_slirp_cidr
  worker_control_url                         = local.worker_control_url
  cas_uri                                    = module.control.cas_uri
  cas_bucket_arn                             = module.control.cas_bucket_arn
  kms_key_arn                                = module.control.kms_key_arn

  secret_arns = {
    checkpoint_encryption_key = module.control.secret_arns.checkpoint_encryption_key
  }

  tags = local.tags
}

module "build_worker" {
  count = var.create_worker ? 1 : 0

  source = "../../modules/worker"

  name                                       = local.build_worker_name
  worker_group_id                            = local.build_worker_group_id
  worker_roles                               = ["build"]
  vpc_id                                     = module.network.vpc_id
  subnet_ids                                 = module.network.private_subnet_ids
  ami_id                                     = var.worker_ami_id
  instance_type                              = coalesce(var.build_worker_instance_type, var.worker_instance_type)
  enable_nested_virtualization               = var.build_worker_enable_nested_virtualization != null ? var.build_worker_enable_nested_virtualization : var.worker_enable_nested_virtualization
  enable_ssm                                 = var.worker_enable_ssm
  launch_lifecycle_heartbeat_timeout_seconds = var.worker_fleet_controller.readiness_timeout_seconds
  min_size                                   = var.build_worker_min_size
  max_size                                   = var.build_worker_max_size
  root_volume_size_gb                        = coalesce(var.build_worker_root_volume_size_gb, var.worker_root_volume_size_gb)
  root_volume_iops                           = coalesce(var.build_worker_root_volume_iops, var.worker_root_volume_iops)
  root_volume_throughput                     = coalesce(var.build_worker_root_volume_throughput, var.worker_root_volume_throughput)
  worker_disk_mib                            = var.build_worker_disk_mib != null ? var.build_worker_disk_mib : var.worker_disk_mib
  worker_disk_reserve_mib                    = local.build_worker_disk_reserve_mib
  vm_vcpus                                   = coalesce(var.build_worker_vm_vcpus, var.worker_vm_vcpus)
  vm_memory_mib                              = coalesce(var.build_worker_vm_memory_mib, var.worker_vm_memory_mib)
  vm_scratch_disk_mib                        = coalesce(var.build_worker_vm_scratch_disk_mib, var.worker_vm_scratch_disk_mib)
  worker_capacity_vcpus                      = var.build_worker_capacity_vcpus != null ? var.build_worker_capacity_vcpus : var.worker_capacity_vcpus
  worker_capacity_memory_mib                 = var.build_worker_capacity_memory_mib != null ? var.build_worker_capacity_memory_mib : var.worker_capacity_memory_mib
  worker_execution_slots                     = var.build_worker_execution_slots != null ? var.build_worker_execution_slots : var.worker_execution_slots
  substrate_cache_max_mib                    = coalesce(var.build_worker_substrate_cache_max_mib, var.worker_substrate_cache_max_mib)
  artifact_cache_max_mib                     = coalesce(var.build_worker_artifact_cache_max_mib, var.worker_artifact_cache_max_mib)
  worker_environment                         = var.worker_environment
  buildkit_slirp_cidr                        = var.worker_buildkit_slirp_cidr
  worker_control_url                         = local.worker_control_url
  cas_uri                                    = module.control.cas_uri
  cas_bucket_arn                             = module.control.cas_bucket_arn
  kms_key_arn                                = module.control.kms_key_arn

  secret_arns = {
    checkpoint_encryption_key = module.control.secret_arns.checkpoint_encryption_key
  }

  tags = local.tags
}

resource "terraform_data" "clickhouse_preconditions" {
  input = {
    create_clickhouse_cloud = var.create_clickhouse_cloud
    clickhouse_url          = local.external_clickhouse_url
  }

  lifecycle {
    precondition {
      condition     = var.create_clickhouse_cloud != (local.external_clickhouse_url != null)
      error_message = "Set exactly one ClickHouse mode: create_clickhouse_cloud=true, or provide clickhouse_url for an external ClickHouse service."
    }

    precondition {
      condition     = !var.create_clickhouse_cloud || var.clickhouse_organization_id != null
      error_message = "clickhouse_organization_id is required when create_clickhouse_cloud is true."
    }

    precondition {
      condition     = var.create_clickhouse_cloud || var.clickhouse_organization_id == null
      error_message = "Do not set clickhouse_organization_id when create_clickhouse_cloud is false; provide clickhouse_url and clickhouse_password_secret_arn for an external ClickHouse service."
    }

    precondition {
      condition     = var.create_clickhouse_cloud || var.clickhouse_cloud_service_name == null
      error_message = "Do not set clickhouse_cloud_service_name when create_clickhouse_cloud is false."
    }

    precondition {
      condition     = var.create_clickhouse_cloud || var.clickhouse_cloud_region == null
      error_message = "Do not set clickhouse_cloud_region when create_clickhouse_cloud is false."
    }

    precondition {
      condition     = var.create_clickhouse_cloud || var.clickhouse_secret_kms_key_id == null
      error_message = "Do not set clickhouse_secret_kms_key_id when create_clickhouse_cloud is false; use clickhouse_password_kms_key_arns for an external ClickHouse password secret."
    }

    precondition {
      condition     = var.create_clickhouse_cloud || var.clickhouse_password_secret_arn != null
      error_message = "clickhouse_password_secret_arn is required when using an external ClickHouse service."
    }

    precondition {
      condition     = !var.create_clickhouse_cloud || var.clickhouse_password_secret_arn == null
      error_message = "Do not set clickhouse_password_secret_arn when create_clickhouse_cloud is true; the ClickHouse module creates the password secret."
    }

    precondition {
      condition     = !var.create_clickhouse_cloud || length(var.clickhouse_password_kms_key_arns) == 0
      error_message = "Do not set clickhouse_password_kms_key_arns when create_clickhouse_cloud is true; use clickhouse_secret_kms_key_id for the managed ClickHouse password secret."
    }

    precondition {
      condition     = !var.create_clickhouse_cloud || var.clickhouse_user == null || var.clickhouse_user == "default"
      error_message = "Terraform-managed ClickHouse Cloud currently configures the default user; leave clickhouse_user unset or set it to default."
    }
  }
}

resource "terraform_data" "control_network_preconditions" {
  input = {
    control_assign_public_ip = var.control_assign_public_ip
    control_image            = var.control_image
    create_control_service   = var.create_control_service
    enable_nat_gateway       = var.enable_nat_gateway
  }

  lifecycle {
    precondition {
      condition     = var.control_assign_public_ip || var.enable_nat_gateway
      error_message = "enable_nat_gateway must be true when control_assign_public_ip is false because private control and migration tasks need outbound access."
    }

    precondition {
      condition     = !var.create_control_service || can(regex("@sha256:[0-9a-f]{64}$", var.control_image))
      error_message = "control_image must be digest-pinned as repository@sha256:<digest> when create_control_service is true."
    }
  }
}

resource "terraform_data" "worker_preconditions" {
  input = {
    create_worker = var.create_worker
  }

  lifecycle {
    precondition {
      condition     = !var.create_worker || var.enable_nat_gateway
      error_message = "enable_nat_gateway must be true when workers run in private subnets."
    }

    precondition {
      condition     = !var.create_worker || var.worker_ami_id != null
      error_message = "worker_ami_id is required when create_worker is true."
    }

    precondition {
      condition     = !var.create_worker || (try(trimspace(local.worker_control_dns_name) != "", false) && try(trimspace(var.certificate_arn) != "", false))
      error_message = "create_worker requires certificate_arn and a private worker control DNS name derived from public_url or cloudfront_origin_domain_name."
    }

    precondition {
      condition     = var.allow_extended_worker_capacity || (var.worker_max_size <= 1 && var.build_worker_max_size <= 1)
      error_message = "dev worker max_size above one requires allow_extended_worker_capacity=true."
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
