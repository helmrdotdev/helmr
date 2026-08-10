mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "111122223333"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }

  mock_resource "aws_secretsmanager_secret" {
    defaults = {
      arn = "arn:aws:secretsmanager:us-east-1:111122223333:secret:mock"
    }
  }

  mock_resource "aws_s3_bucket" {
    defaults = {
      arn = "arn:aws:s3:::mock-bucket"
    }
  }

  mock_resource "aws_kms_key" {
    defaults = {
      arn = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::111122223333:role/mock"
    }
  }

  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::111122223333:policy/mock"
    }
  }

  mock_resource "aws_lb" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/app/mock/0000000000000000"
    }
  }

  mock_resource "aws_lb_target_group" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/mock/0000000000000000"
    }
  }

  mock_resource "aws_elasticache_replication_group" {
    defaults = {
      primary_endpoint_address = "redis.example.test"
    }
  }

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

mock_provider "http" {}
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

override_module {
  target = module.release_artifacts
  outputs = {
    controlplane_image = "111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    worker_ami_id      = "ami-00000000000000000"
    manifest_url       = null
  }
}

variables {
  aws_region                        = "us-east-1"
  worker_group_name                 = "workers"
  region_id                         = "us-east-1"
  platform_store_uri                = "s3://helmr-test-platform/objects"
  platform_store_bucket_arn         = "arn:aws:s3:::helmr-test-platform"
  platform_store_kms_key_arn        = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
  build_policy_digest               = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  clickhouse_url                    = "https://clickhouse.example.test:8443"
  helmr_version                     = "v0.0.0-test"
  github_oauth_client_id            = "github-client"
  worker_network_blocked_ipv4_cidrs = ["10.0.0.0/8", "169.254.0.0/16"]
  public_url                        = "https://helmr.example.test"
  certificate_arn                   = "arn:aws:acm:us-east-1:111122223333:certificate/00000000-0000-0000-0000-000000000000"
  controlplane_image                = "111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  worker_ami_id                     = "ami-00000000000000000"

  create_worker                       = false
  worker_instance_type                = "c8i.xlarge"
  worker_enable_nested_virtualization = true
  worker_capacity_vcpus               = 8
  worker_capacity_memory_mib          = 16384
  worker_execution_slots              = 4
  worker_disk_mib                     = 262144
  worker_disk_reserve_mib             = 1024
  worker_vm_vcpus                     = 2
  worker_vm_memory_mib                = 4096
  worker_vm_scratch_disk_mib          = 32768
  worker_substrate_cache_max_mib      = 32768
  worker_artifact_cache_max_mib       = 16384

  build_worker_instance_type                = "c8i.2xlarge"
  build_worker_enable_nested_virtualization = true
  build_worker_capacity_vcpus               = 12
  build_worker_capacity_memory_mib          = 32768
  build_worker_execution_slots              = 1
  build_worker_disk_mib                     = 196608
  build_worker_disk_reserve_mib             = 2048
  build_worker_vm_vcpus                     = 3
  build_worker_vm_memory_mib                = 4096
  build_worker_vm_scratch_disk_mib          = 32768
  build_worker_substrate_cache_max_mib      = 24576
  build_worker_artifact_cache_max_mib       = 8192
}

run "baseline_pool_generations" {
  command = plan

  assert {
    condition = (
      local.worker_pool_names.run == "run-a43e4d20d66bf326c6b63a03df5d0c0595d2cb3b9065af598bafd45caa7bb895" &&
      local.worker_pool_names.build == "build-e1ad5f21cd843f59d78827511dddade65405b186f72d434b6a5a5958c52fa311"
    )
    error_message = "Worker Pool generation names must be the canonical role-prefixed SHA-256 digests."
  }

  assert {
    condition = alltrue([
      for name in values(local.worker_pool_names) :
      length(name) <= 128 && can(regex("^[a-z0-9]([a-z0-9-]{0,126}[a-z0-9])?$", name))
    ])
    error_message = "Generated Worker Pool names must satisfy the Product canonical-name contract."
  }
}

run "derived_guest_disk_changes_generation" {
  command = plan

  variables {
    worker_artifact_cache_max_mib = 8192
  }

  assert {
    condition = (
      local.worker_pool_names.run != "run-a43e4d20d66bf326c6b63a03df5d0c0595d2cb3b9065af598bafd45caa7bb895" &&
      local.worker_pool_names.build == "build-e1ad5f21cd843f59d78827511dddade65405b186f72d434b6a5a5958c52fa311"
    )
    error_message = "A changed effective run guest disk must create a new run Pool generation without rotating the independently configured build Pool."
  }
}

run "build_template_input_changes_generation" {
  command = plan

  variables {
    build_worker_capacity_memory_mib = 65536
  }

  assert {
    condition = (
      local.worker_pool_names.run == "run-a43e4d20d66bf326c6b63a03df5d0c0595d2cb3b9065af598bafd45caa7bb895" &&
      local.worker_pool_names.build != "build-e1ad5f21cd843f59d78827511dddade65405b186f72d434b6a5a5958c52fa311"
    )
    error_message = "A changed build WorkerTemplate input must create a new build Pool generation without rotating the run Pool."
  }
}

run "build_per_vm_overrides_change_generation" {
  command = plan

  variables {
    build_worker_vm_vcpus            = 4
    build_worker_vm_memory_mib       = 8192
    build_worker_vm_scratch_disk_mib = 40960
  }

  assert {
    condition = (
      local.worker_pool_generation_inputs.build.per_vm.cpu_millis == 4000 &&
      local.worker_pool_generation_inputs.build.per_vm.memory_mib == 8192 &&
      local.worker_pool_generation_inputs.build.per_vm.guest_ephemeral_disk_mib == 40960 &&
      local.worker_pool_names.run == "run-a43e4d20d66bf326c6b63a03df5d0c0595d2cb3b9065af598bafd45caa7bb895" &&
      local.worker_pool_names.build != "build-e1ad5f21cd843f59d78827511dddade65405b186f72d434b6a5a5958c52fa311"
    )
    error_message = "Changed effective build per-VM overrides must create a new build Pool generation without rotating the run Pool."
  }
}

run "cache_redistribution_does_not_change_generation" {
  command = plan

  variables {
    worker_substrate_cache_max_mib = 24576
    worker_artifact_cache_max_mib  = 24576
  }

  assert {
    condition = (
      local.worker_pool_names.run == "run-a43e4d20d66bf326c6b63a03df5d0c0595d2cb3b9065af598bafd45caa7bb895" &&
      local.worker_pool_names.build == "build-e1ad5f21cd843f59d78827511dddade65405b186f72d434b6a5a5958c52fa311"
    )
    error_message = "Redistributing cache limits without changing effective guest disk must not rotate Worker Pool generations."
  }
}

run "scale_policy_does_not_change_generation" {
  command = plan

  variables {
    worker_min_size       = 5
    worker_max_size       = 9
    build_worker_min_size = 2
    build_worker_max_size = 7
  }

  assert {
    condition = (
      local.worker_pools.run.min_size == 5 &&
      local.worker_pools.run.max_size == 9 &&
      local.worker_pools.build.min_size == 2 &&
      local.worker_pools.build.max_size == 7 &&
      local.worker_pool_names.run == "run-a43e4d20d66bf326c6b63a03df5d0c0595d2cb3b9065af598bafd45caa7bb895" &&
      local.worker_pool_names.build == "build-e1ad5f21cd843f59d78827511dddade65405b186f72d434b6a5a5958c52fa311"
    )
    error_message = "Mutable ASG minimum and maximum sizes must not rotate Worker Pool generations."
  }
}
