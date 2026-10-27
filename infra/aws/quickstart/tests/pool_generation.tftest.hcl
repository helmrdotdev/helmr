mock_provider "aws" {
  mock_resource "aws_ecs_cluster" { defaults = { arn = "arn:aws:ecs:us-east-1:000000000000:cluster/mock" } }
  mock_resource "aws_ecs_task_definition" { defaults = { arn = "arn:aws:ecs:us-east-1:000000000000:task-definition/mock:1" } }
  mock_resource "aws_cloudwatch_event_rule" { defaults = { arn = "arn:aws:events:us-east-1:000000000000:rule/mock" } }
  mock_data "aws_region" { defaults = { region = "us-east-1" } }
  mock_data "aws_caller_identity" { defaults = { account_id = "111122223333" } }
  mock_data "aws_partition" { defaults = { partition = "aws", dns_suffix = "amazonaws.com" } }
  mock_data "aws_vpc" { defaults = { cidr_block = "10.81.0.0/16" } }
  mock_resource "aws_secretsmanager_secret" { defaults = { arn = "arn:aws:secretsmanager:us-east-1:111122223333:secret:mock" } }
  mock_resource "aws_s3_bucket" { defaults = { arn = "arn:aws:s3:::mock-bucket" } }
  mock_resource "aws_kms_key" { defaults = { arn = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000" } }
  mock_resource "aws_iam_role" { defaults = { arn = "arn:aws:iam::111122223333:role/mock" } }
  mock_resource "aws_iam_policy" { defaults = { arn = "arn:aws:iam::111122223333:policy/mock" } }
  mock_resource "aws_launch_template" { defaults = { id = "lt-0123456789abcdef0", latest_version = 1 } }
  mock_resource "aws_lb" { defaults = { arn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/app/mock/0000000000000000" } }
  mock_resource "aws_lb_target_group" { defaults = { arn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/mock/0000000000000000" } }
  mock_resource "aws_db_instance" {
    defaults = {
      address = "database.example.test"
      port    = 5432
      master_user_secret = [{
        kms_key_id    = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
        secret_arn    = "arn:aws:secretsmanager:us-east-1:111122223333:secret:database-master"
        secret_status = "active"
      }]
    }
  }
}

mock_provider "http" {
  mock_data "http" {
    defaults = {
      status_code   = 200
      response_body = "{\"schema\":\"helmr.aws-release.v0\",\"controlplaneImage\":\"111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"platformRelease\":{},\"workerImage\":{\"amis\":{\"us-east-1\":\"ami-00000000000000000\"}}}"
    }
  }
}
mock_provider "random" {}

override_module {
  target = module.controlplane_network
  outputs = {
    vpc_id             = "vpc-control"
    public_subnet_ids  = ["subnet-control-public-a", "subnet-control-public-b"]
    private_subnet_ids = ["subnet-control-private-a", "subnet-control-private-b"]
  }
}

override_module {
  target = module.execution_network
  outputs = {
    vpc_id             = "vpc-execution"
    public_subnet_ids  = ["subnet-execution-public-a", "subnet-execution-public-b"]
    private_subnet_ids = ["subnet-execution-private-a", "subnet-execution-private-b"]
  }
}

variables {
  controlplane_idle_timeout_seconds = 240
  worker_environment = {
    WORKER_LOG_CHUNK_BYTES    = "65536"
    WORKER_LOG_BUFFER_BYTES   = "1048576"
    WORKER_LOG_BUFFER_RECORDS = "256"
  }
  audit_retention_seconds = 2592000
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
    DIAGNOSTIC_CHUNK_BYTES                 = "32768"
    DIAGNOSTIC_SOURCE_BYTES                = "4194304"
    DIAGNOSTIC_SOURCE_RECORDS              = "1024"
    DIAGNOSTIC_ENVIRONMENT_BYTES           = "16777216"
    DIAGNOSTIC_ENVIRONMENT_RECORDS         = "4096"
    DIAGNOSTIC_QUEUE_BYTES                 = "67108864"
    DIAGNOSTIC_QUEUE_RECORDS               = "16384"
    DIAGNOSTIC_DB_MAX_CONNECTIONS          = "2"
  }
  dispatcher_environment = {
    DIAGNOSTIC_EXPORT_BATCH_BYTES   = "8388608"
    DIAGNOSTIC_EXPORT_BATCH_RECORDS = "1024"
    DIAGNOSTIC_CHUNK_BYTES          = "65536"
    DIAGNOSTIC_SOURCE_BYTES         = "4194304"
    DIAGNOSTIC_SOURCE_RECORDS       = "1024"
    DIAGNOSTIC_ENVIRONMENT_BYTES    = "16777216"
    DIAGNOSTIC_ENVIRONMENT_RECORDS  = "4096"
    DIAGNOSTIC_QUEUE_BYTES          = "67108864"
    DIAGNOSTIC_QUEUE_RECORDS        = "16384"
    DIAGNOSTIC_DB_MAX_CONNECTIONS   = "2"
  }
  enable_nat_gateway                       = true
  worker_computer_devices                  = ["/dev/nbd0", "/dev/nbd1"]
  aws_region                               = "us-east-1"
  worker_group_name                        = "workers"
  region_id                                = "us-east-1"
  platform_store_uri                       = "s3://helmr-test-platform/objects"
  platform_store_bucket_arn                = "arn:aws:s3:::helmr-test-platform"
  platform_store_kms_key_arn               = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
  clickhouse_url                           = "https://clickhouse.example.test:8443"
  clickhouse_access_mode                   = "external"
  clickhouse_reader_user                   = "telemetry_reader"
  clickhouse_reader_password_secret_arn    = "arn:aws:secretsmanager:us-east-1:000000000000:secret:telemetry-reader"
  clickhouse_ingester_user                 = "telemetry_ingester"
  clickhouse_ingester_password_secret_arn  = "arn:aws:secretsmanager:us-east-1:000000000000:secret:telemetry-ingester"
  clickhouse_migration_user                = "telemetry_migration"
  clickhouse_migration_password_secret_arn = "arn:aws:secretsmanager:us-east-1:000000000000:secret:telemetry-migration"
  helmr_version                            = "v0.0.0-test"
  github_oauth_client_id                   = "github-client"
  worker_network_blocked_ipv4_cidrs        = ["10.0.0.0/8", "10.81.0.0/16", "169.254.0.0/16"]
  app_origin                               = "https://helmr.example.test"
  controlplane_image                       = "111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  worker_ami_id                            = "ami-00000000000000000"

  worker_count                        = 0
  create_worker                       = false
  worker_host_type                    = "c8i.xlarge"
  worker_enable_nested_virtualization = true
  worker_capacity_vcpus               = 8
  worker_capacity_memory_mib          = 16676
  worker_execution_slots              = 4
  worker_root_volume_size_gb          = 2048
  worker_disk_mib                     = 2097152
  worker_disk_reserve_mib             = 1024
  worker_vm_vcpus                     = 2
  worker_vm_memory_mib                = 4096
  worker_vm_scratch_disk_mib          = 32768
}

run "baseline_execution_generation" {
  command = apply
  variables {
    enable_nat_gateway = true
    create_worker      = true
    worker_count       = 0
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
    enable_nat_gateway = true
    create_worker      = true
    worker_count       = 0
  }
}
run "retain_current_sealed_generation" {
  command = plan
  variables {
    enable_nat_gateway          = true
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
run "rollback_default" {
  command = plan
  variables {
    create_controlplane_service = true
    certificate_arn             = "arn:aws:acm:us-east-1:111122223333:certificate/00000000-0000-0000-0000-000000000001"
  }
}
run "rollback_disabled" {
  command = plan
  variables {
    create_controlplane_service = true
    certificate_arn             = "arn:aws:acm:us-east-1:111122223333:certificate/00000000-0000-0000-0000-000000000001"
    enable_deployment_rollback  = false
  }
}

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
    create_worker             = true
    worker_count              = 2
    capacity_token_secret_arn = "arn:aws:secretsmanager:us-east-1:111122223333:secret:capacity-token"
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
      ADMIN_EMAILS                           = "operator@example.test"
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

run "slack_no_global_credentials" {
  command = plan
}

run "diagnostic_environment" {
  command = plan
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
