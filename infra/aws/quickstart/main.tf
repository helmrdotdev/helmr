locals {
  name = lower(var.name)
  blocked_cidrs_by_order = {
    for cidr in distinct(concat(
      [var.vpc_cidr, "169.254.0.0/16", "${aws_eip.control.public_ip}/32"],
      var.worker_network_blocked_ipv4_cidrs
    )) : "${join(".", [for octet in split(".", split("/", cidr)[0]) : format("%03d", tonumber(octet))])}/${format("%02d", tonumber(split("/", cidr)[1]))}" => cidr
  }
  worker_network_blocked_ipv4_cidrs = [for key in sort(keys(local.blocked_cidrs_by_order)) : local.blocked_cidrs_by_order[key]]
  worker_controlplane_url           = var.app_origin
  worker_ami_id                     = coalesce(var.worker_ami_id, module.release_artifacts.worker_ami_id, "ami-unconfigured")
  worker_provider_contract_digest = sha256(jsonencode({
    module   = filesha256("${path.module}/../modules/worker/main.tf")
    inputs   = filesha256("${path.module}/../modules/worker/variables.tf")
    userdata = filesha256("${path.module}/../modules/worker/templates/user-data.sh.tftpl")
  }))

  worker_guest_ephemeral_disk_mib = coalesce(var.worker_execution_slots, 0) * var.worker_vm_scratch_disk_mib
  worker_generation_inputs = {
    generation_key        = var.worker_generation_key
    ami_id                = local.worker_ami_id
    instance_type         = var.worker_host_type
    nested_virtualization = var.worker_enable_nested_virtualization
    supply = {
      computer = {
        devices = var.worker_computer_devices
      }
      environment     = var.worker_environment
      contract_digest = local.worker_provider_contract_digest
      enable_ssm      = var.worker_enable_ssm
      network = {
        blocked_ipv4_cidrs     = local.worker_network_blocked_ipv4_cidrs
        private_business_cidrs = var.worker_network_private_business_cidrs
        link_pool              = var.worker_network_link_pool
        resolver_ipv4          = coalesce(var.worker_network_resolver_ipv4, cidrhost(var.vpc_cidr, 2))
        translation_pool       = var.worker_network_translation_pool
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
        total_mib            = var.worker_disk_mib
        reserve_mib          = var.worker_disk_reserve_mib
        computer_staging_mib = var.worker_computer_staging_mib
      }
      lifecycle = {
        health_check_grace_period_seconds               = 900
        launch_lifecycle_heartbeat_timeout_seconds      = var.worker_launch_timeout_seconds
        termination_lifecycle_heartbeat_timeout_seconds = 180
        termination_wait_timeout_seconds                = 1800
        lifecycle_heartbeat_interval_seconds            = 60
        termination_policies                            = ["OldestLaunchTemplate", "OldestInstance"]
        protect_from_scale_in                           = true
        health_check_type                               = "EC2"
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
      count                      = var.worker_count
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
      count                        = spec.count
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
      computer_staging_mib         = spec.generation_inputs.supply.disk.computer_staging_mib
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

module "worker_group" {
  for_each = var.create_worker ? toset(keys(local.worker_generation_specs)) : toset([])

  source             = "../modules/worker"
  computer_devices   = local.worker_generations[each.key].generation_inputs.supply.computer.devices
  worker_environment = local.worker_generations[each.key].generation_inputs.supply.environment

  name                                            = local.worker_generations[each.key].provider_name
  worker_pool_name                                = each.key
  network_private_business_cidrs                  = local.worker_generations[each.key].generation_inputs.supply.network.private_business_cidrs
  network_blocked_ipv4_cidrs                      = local.worker_generations[each.key].generation_inputs.supply.network.blocked_ipv4_cidrs
  network_link_pool                               = local.worker_generations[each.key].generation_inputs.supply.network.link_pool
  network_translation_pool                        = local.worker_generations[each.key].generation_inputs.supply.network.translation_pool
  network_resolver_ipv4                           = local.worker_generations[each.key].generation_inputs.supply.network.resolver_ipv4
  vpc_id                                          = aws_vpc.quickstart.id
  subnet_ids                                      = [aws_subnet.public.id]
  ami_id                                          = local.worker_generations[each.key].ami_id
  instance_type                                   = local.worker_generations[each.key].instance_type
  enable_nested_virtualization                    = local.worker_generations[each.key].enable_nested_virtualization
  enable_ssm                                      = local.worker_generations[each.key].generation_inputs.supply.enable_ssm
  sealed_provider_definition                      = local.worker_generations[each.key].sealed_provider_definition
  health_check_grace_period_seconds               = local.worker_generations[each.key].lifecycle.health_check_grace_period_seconds
  launch_lifecycle_heartbeat_timeout_seconds      = local.worker_generations[each.key].lifecycle.launch_lifecycle_heartbeat_timeout_seconds
  termination_lifecycle_heartbeat_timeout_seconds = local.worker_generations[each.key].lifecycle.termination_lifecycle_heartbeat_timeout_seconds
  termination_wait_timeout_seconds                = local.worker_generations[each.key].lifecycle.termination_wait_timeout_seconds
  lifecycle_heartbeat_interval_seconds            = local.worker_generations[each.key].lifecycle.lifecycle_heartbeat_interval_seconds
  min_size                                        = 0
  max_size                                        = local.worker_generations[each.key].count
  desired_capacity                                = local.worker_generations[each.key].count
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
  computer_staging_mib                            = local.worker_generations[each.key].computer_staging_mib
  worker_controlplane_url                         = local.worker_controlplane_url
  cas_uri                                         = "s3://${aws_s3_bucket.cas.bucket}"
  cas_bucket_arn                                  = aws_s3_bucket.cas.arn
  kms_key_arn                                     = aws_kms_key.helmr.arn
  platform_store_uri                              = local.worker_generations[each.key].generation_inputs.supply.platform_store.uri
  platform_store_bucket_arn                       = local.worker_generations[each.key].generation_inputs.supply.platform_store.bucket_arn
  platform_store_kms_key_arn                      = local.worker_generations[each.key].generation_inputs.supply.platform_store.kms_key_arn

  secret_arns = {
    checkpoint_encryption_key = aws_secretsmanager_secret.helmr["checkpoint_encryption_key"].arn
    worker_enrollment_token   = aws_secretsmanager_secret.helmr["worker_enrollment_token"].arn
  }

  tags = local.tags
}

resource "terraform_data" "quickstart_preconditions" {
  lifecycle {
    precondition {
      condition     = !contains(keys(var.retained_worker_generations), local.worker_pool_name)
      error_message = "A retained generation must differ from the current generation. Keep the source unchanged while preparing a new target."
    }
  }
}
