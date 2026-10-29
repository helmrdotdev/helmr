mock_provider "aws" {
  mock_data "aws_region" { defaults = { region = "us-east-1" } }
  mock_data "aws_availability_zones" { defaults = { names = ["us-east-1a"] } }
  mock_data "aws_caller_identity" { defaults = { account_id = "111122223333" } }
  mock_data "aws_partition" { defaults = { partition = "aws", dns_suffix = "amazonaws.com" } }
  mock_data "aws_vpc" { defaults = { cidr_block = "10.80.0.0/16" } }
  mock_resource "aws_eip" { defaults = { public_ip = "3.112.1.2" } }
  mock_resource "aws_secretsmanager_secret" { defaults = { arn = "arn:aws:secretsmanager:us-east-1:111122223333:secret:mock" } }
  mock_resource "aws_s3_bucket" { defaults = { arn = "arn:aws:s3:::mock-bucket" } }
  mock_resource "aws_kms_key" { defaults = { arn = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000" } }
  mock_resource "aws_iam_role" { defaults = { arn = "arn:aws:iam::111122223333:role/mock" } }
  mock_resource "aws_iam_policy" { defaults = { arn = "arn:aws:iam::111122223333:policy/mock" } }
  mock_resource "aws_launch_template" { defaults = { id = "lt-0123456789abcdef0", latest_version = 1 } }
}
mock_provider "http" {}

variables {
  aws_region                 = "us-east-1"
  control_ami_id             = "ami-00000000000000000"
  region_id                  = "us-east-1"
  platform_store_uri         = "s3://helmr-test-platform/objects"
  platform_store_bucket_arn  = "arn:aws:s3:::helmr-test-platform"
  platform_store_kms_key_arn = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
  helmr_version              = "v0.0.0-test"
  github_oauth_client_id     = "github-client"
  app_origin                 = "https://helmr.example.test"
  controlplane_image         = "ghcr.io/helmrdotdev/helmr-control-plane@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  worker_ami_id              = "ami-00000000000000000"
}

run "compact_baseline" {
  command = apply
  assert {
    condition = (
      length(module.worker_group) == 0 &&
      aws_subnet.public.map_public_ip_on_launch &&
      aws_instance.control.metadata_options[0].http_tokens == "required" &&
      aws_instance.control.root_block_device[0].delete_on_termination &&
      alltrue([for secret in aws_secretsmanager_secret.helmr : secret.recovery_window_in_days == 0])
    )
    error_message = "Quickstart must initialize a compact host without Workers or retained disks/secrets."
  }
  assert {
    condition     = alltrue([for cidr in ["10.80.0.0/16", "169.254.0.0/16", "3.112.1.2/32"] : contains(local.worker_network_blocked_ipv4_cidrs, cidr)])
    error_message = "Guest management deny policy must include VPC, metadata and the public application address."
  }
}

run "compact_worker" {
  command = plan
  variables { create_worker = true }
  assert {
    condition = (
      var.worker_count == 1 && var.worker_execution_slots == 1 &&
      var.worker_vm_vcpus == 1 && var.worker_vm_memory_mib == 2048
    )
    error_message = "Quickstart defaults must fund one small execution slot."
  }
}

run "baseline_execution_generation" {
  command = apply
  variables {

    create_worker = true
    worker_count  = 0
  }

  assert {
    condition = (
      local.worker_pool_name == "execution-${sha256(jsonencode(local.worker_generation_inputs))}" &&
      length(local.worker_pool_name) == 74 &&
      can(regex("^execution-[0-9a-f]{64}$", local.worker_pool_name)) &&
      length(output.worker_generation_definitions) == 1 &&
      contains(keys(output.worker_generation_definitions), local.worker_pool_name)
    )
    error_message = "one canonical execution Pool generation must be derived from the complete immutable supply inputs"
  }
}

run "immutable_capacity_change_rotates_generation" {
  command = plan

  variables {
    worker_vm_scratch_disk_mib  = 40960
    retained_worker_generations = run.baseline_execution_generation.worker_generation_definitions
  }

  assert {
    condition     = local.worker_pool_name != one(keys(var.retained_worker_generations))
    error_message = "an immutable execution-shape change must rotate the Pool generation"
  }
}

run "count_change_does_not_rotate_generation" {
  command = plan

  variables {
    worker_count = 5
  }

  assert {
    condition = (
      one(values(output.worker_generation_definitions)).count == 5
    )
    error_message = "mutable ASG size must not rotate the execution Pool generation"
  }
}

run "export_current_sealed_generation" {
  command = apply
  variables {

    create_worker = true
    worker_count  = 0
  }
}
run "retain_current_sealed_generation" {
  command = plan
  variables {

    worker_vm_scratch_disk_mib  = 40960
    create_worker               = true
    worker_computer_devices     = ["/dev/nbd0", "/dev/nbd1", "/dev/nbd2"]
    retained_worker_generations = run.export_current_sealed_generation.worker_generation_definitions
  }
  assert {
    condition     = alltrue([for key, generation in var.retained_worker_generations : jsonencode(output.worker_generation_definitions[key].sealed_provider_definition) == jsonencode(generation.sealed_provider_definition) && generation.sealed_provider_definition.boundary_policy_arn == null])
    error_message = "A newly emitted self-hosted definition must round-trip with its explicit null boundary ARN."
  }
}

# Compiled service values are checked by infra/aws/tests/check_root_composition.py.


run "device_allowlist_rotates_generation" {
  command = plan
  variables {
    worker_computer_devices     = ["/dev/nbd0", "/dev/nbd1", "/dev/nbd2"]
    retained_worker_generations = run.export_current_sealed_generation.worker_generation_definitions
  }
  assert {
    condition     = local.worker_pool_name != one(keys(var.retained_worker_generations))
    error_message = "Changing Computer devices must allocate a new Pool."
  }
}

run "release_tag_accepts_stable" {
  command = plan
  variables { helmr_version = "v1.2.3" }
}

run "release_tag_accepts_prerelease" {
  command = plan
  variables { helmr_version = "v1.2.3-rc.1" }
}

run "release_tag_rejects_latest" {
  command = plan
  variables { helmr_version = "latest" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_range" {
  command = plan
  variables { helmr_version = ">=1.2.3" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_missing_prefix" {
  command = plan
  variables { helmr_version = "1.2.3" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_missing_patch" {
  command = plan
  variables { helmr_version = "v1.2" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_leading_zero" {
  command = plan
  variables { helmr_version = "v01.2.3" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_prerelease_leading_zero" {
  command = plan
  variables { helmr_version = "v1.2.3-01" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_build_metadata" {
  command = plan
  variables { helmr_version = "v1.2.3+build.1" }
  expect_failures = [var.helmr_version]
}

run "release_tag_rejects_trailing_text" {
  command = plan
  variables { helmr_version = "v1.2.3junk" }
  expect_failures = [var.helmr_version]
}

run "fixed_capacity_plan" {
  command = plan
  variables {
    create_worker = true
    worker_count  = 2

    controlplane_environment = {
      DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY      = "24"
      DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY = "18"
      DEPLOYMENT_UPLOAD_IDLE_TIMEOUT             = "2m"
      DEPLOYMENT_UPLOAD_MAX_DURATION             = "2h"

      ENVIRONMENT_MAX_RESIDENT_COMPUTERS     = "10"
      ENVIRONMENT_MAX_CPU_MILLIS             = "16000"
      ENVIRONMENT_MAX_MEMORY_BYTES           = "68719476736"
      ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES = "1099511627776"
      ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS = "100"
      ENVIRONMENT_MAX_CAUSAL_DEPTH           = "8"
      ENVIRONMENT_ADMISSION_RATE_PER_SECOND  = "10"
      ENVIRONMENT_ADMISSION_BURST            = "20"
      ENVIRONMENT_PREPARATION_TIMEOUT_MS     = "600000"
    }
  }
}

run "export_serving_source" {
  command = apply
  variables {
    create_worker = true
    worker_count  = 2
  }
}

run "prepare_inert_target" {
  command = plan
  variables {
    create_worker               = true
    worker_count                = 0
    worker_ami_id               = "ami-11111111111111111"
    retained_worker_generations = run.export_serving_source.worker_generation_definitions
  }
  assert {
    condition = (
      local.worker_pool_name != one(keys(var.retained_worker_generations)) &&
      output.worker_generation_definitions[local.worker_pool_name].count == 0 &&
      output.worker_generation_definitions[one(keys(var.retained_worker_generations))].count == 2
    )
    error_message = "Target preparation must retain the serving source count and prepare a distinct zero-capacity generation."
  }
}

run "activate_after_full_stop" {
  command = plan
  variables {
    create_worker = true
    worker_count  = 1
    worker_ami_id = "ami-11111111111111111"
    retained_worker_generations = {
      for name, generation in run.export_serving_source.worker_generation_definitions : name => {
        generation_inputs          = generation.generation_inputs
        count                      = 0
        sealed_provider_definition = generation.sealed_provider_definition
      }
    }
  }
}

run "worker_count_must_be_explicit" {
  command = plan
  variables {
    create_worker = true
    worker_count  = null
  }
  expect_failures = [var.worker_count]
}

run "worker_count_rejects_fraction" {
  command = plan
  variables { worker_count = 1.5 }
  expect_failures = [var.worker_count]
}


run "same_build_recovery_has_fresh_binding" {
  command = plan
  variables {
    create_worker               = true
    worker_count                = 0
    worker_generation_key       = "recovery-1"
    retained_worker_generations = run.export_serving_source.worker_generation_definitions
  }
  assert {
    condition = (
      local.worker_pool_name != one(keys(var.retained_worker_generations)) &&
      output.worker_generation_definitions[local.worker_pool_name].generation_inputs.ami_id ==
      var.retained_worker_generations[one(keys(var.retained_worker_generations))].generation_inputs.ami_id &&
      output.worker_generation_definitions[one(keys(var.retained_worker_generations))].count == 2
    )
    error_message = "Same-build recovery must prepare a distinct inert Pool without changing the source count or release."
  }
}

run "reject_retained_current_generation_collision" {
  command = plan
  variables {
    create_worker               = true
    worker_count                = 0
    retained_worker_generations = run.export_serving_source.worker_generation_definitions
  }
  expect_failures = [terraform_data.quickstart_preconditions]
}

run "computer_staging_rotates_and_retains_generation" {
  command = plan
  variables {
    create_worker               = true
    worker_computer_staging_mib = 32768
    retained_worker_generations = run.export_serving_source.worker_generation_definitions
  }
  assert {
    condition = (
      local.worker_pool_name != one(keys(var.retained_worker_generations)) &&
      local.worker_generation_inputs.capacity.guest_ephemeral_disk_mib == var.worker_execution_slots * var.worker_vm_scratch_disk_mib &&
      one(values(var.retained_worker_generations)).generation_inputs.supply.disk.computer_staging_mib == 65536 &&
      local.worker_generation_inputs.supply.disk.computer_staging_mib == 32768
    )
    error_message = "Staging must rotate the Pool, preserve retained staging, and keep guest capacity separate from host files."
  }
}



run "worker_log_environment_rotates_and_retains_generation" {
  command = plan
  variables {
    create_worker = true
    worker_count  = 1
    worker_environment = {
      WORKER_LOG_CHUNK_BYTES    = "65536"
      WORKER_LOG_BUFFER_BYTES   = "2097152"
      WORKER_LOG_BUFFER_RECORDS = "256"
    }
    retained_worker_generations = run.export_serving_source.worker_generation_definitions
  }
  assert {
    condition = (
      local.worker_pool_name != one(keys(var.retained_worker_generations)) &&
      one(values(var.retained_worker_generations)).generation_inputs.supply.environment.WORKER_LOG_BUFFER_BYTES == "1048576" &&
      local.worker_generation_inputs.supply.environment.WORKER_LOG_BUFFER_BYTES == "2097152"
    )
    error_message = "Log environment changes must rotate the Pool while retaining the source generation's exact environment."
  }
}
