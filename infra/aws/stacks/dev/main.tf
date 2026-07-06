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

  tags = {
    Project     = "helmr"
    Environment = "dev"
    ManagedBy   = "terraform"
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

module "worker" {
  count = var.create_worker ? 1 : 0

  source = "../../modules/worker"

  name                         = var.name
  vpc_id                       = module.network.vpc_id
  subnet_ids                   = module.network.private_subnet_ids
  ami_id                       = var.worker_ami_id
  instance_type                = var.worker_instance_type
  enable_nested_virtualization = var.worker_enable_nested_virtualization
  enable_ssm                   = var.worker_enable_ssm
  desired_capacity             = var.worker_desired_capacity
  min_size                     = var.worker_min_size
  max_size                     = var.worker_max_size
  root_volume_size_gb          = var.worker_root_volume_size_gb
  root_volume_iops             = var.worker_root_volume_iops
  root_volume_throughput       = var.worker_root_volume_throughput
  worker_disk_mib              = var.worker_disk_mib
  vm_vcpus                     = var.worker_vm_vcpus
  vm_memory_mib                = var.worker_vm_memory_mib
  vm_scratch_disk_mib          = var.worker_vm_scratch_disk_mib
  worker_capacity_vcpus        = var.worker_capacity_vcpus
  worker_capacity_memory_mib   = var.worker_capacity_memory_mib
  worker_execution_slots       = var.worker_execution_slots
  worker_environment           = var.worker_environment
  buildkit_slirp_cidr          = var.worker_buildkit_slirp_cidr
  worker_control_url           = local.worker_control_url
  cas_uri                      = module.control.cas_uri
  cas_bucket_arn               = module.control.cas_bucket_arn
  kms_key_arn                  = module.control.kms_key_arn

  secret_arns = {
    worker_bootstrap_token    = module.control.secret_arns.worker_bootstrap_token
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
      condition     = !var.create_worker || var.worker_desired_capacity == 0 || var.enable_nat_gateway
      error_message = "enable_nat_gateway must be true when worker_desired_capacity is greater than zero because dev workers run in private subnets and need outbound access."
    }

    precondition {
      condition     = !var.create_worker || var.worker_ami_id != null
      error_message = "worker_ami_id is required when create_worker is true."
    }

    precondition {
      condition     = !var.create_worker || (try(trimspace(local.worker_control_dns_name) != "", false) && try(trimspace(var.certificate_arn) != "", false))
      error_message = "create_worker requires certificate_arn and a private worker control DNS name derived from public_url or cloudfront_origin_domain_name."
    }
  }
}
