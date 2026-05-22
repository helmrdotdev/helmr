locals {
  name                     = lower(var.name)
  public_url_host          = var.public_url == null ? null : regex("^https?://([^/:]+)", var.public_url)[0]
  worker_control_dns_name  = var.enable_cloudfront ? var.cloudfront_origin_domain_name : local.public_url_host
  private_control_dns_name = var.create_worker ? local.worker_control_dns_name : null
  worker_control_url       = module.control.private_control_url

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
  github_app_id                          = var.github_app_id
  github_app_slug                        = var.github_app_slug
  github_app_client_id                   = var.github_app_client_id
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

module "worker" {
  count = var.create_worker ? 1 : 0

  source = "../modules/worker"

  name                         = local.name
  vpc_id                       = module.network.vpc_id
  subnet_ids                   = module.network.private_subnet_ids
  ami_id                       = module.release_artifacts.worker_ami_id
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

resource "terraform_data" "quickstart_preconditions" {
  input = {
    control_assign_public_ip = var.control_assign_public_ip
    create_worker            = var.create_worker
    cloudfront_origin        = var.cloudfront_origin_domain_name
    enable_cloudfront        = var.enable_cloudfront
    enable_nat_gateway       = var.enable_nat_gateway
    public_url               = var.public_url
    worker_ami_id            = module.release_artifacts.worker_ami_id
    worker_desired_capacity  = var.worker_desired_capacity
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
      condition     = !var.create_worker || (var.worker_min_size <= var.worker_desired_capacity && var.worker_desired_capacity <= var.worker_max_size)
      error_message = "worker_min_size must be <= worker_desired_capacity, and worker_desired_capacity must be <= worker_max_size."
    }
  }
}
