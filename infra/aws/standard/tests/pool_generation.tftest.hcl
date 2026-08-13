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

  mock_data "aws_vpc" {
    defaults = {
      cidr_block = "10.91.0.0/16"
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

  mock_resource "aws_launch_template" {
    defaults = {
      id             = "lt-0123456789abcdef0"
      latest_version = 1
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

mock_provider "http" {
  mock_data "http" {
    defaults = {
      status_code   = 200
      response_body = "{\"controlplane_image\":\"111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"worker_amis\":{\"us-east-1\":\"ami-00000000000000000\"}}"
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
      local.worker_pool_names.run == "run-a3e8694369204dabbbb71776cc00e4493a8481fd6578d0155cf6641477598aac" &&
      local.worker_pool_names.build == "build-b434d01fbfaf692968c80748167e3c2856f65e7d2e373241dbf5da55350d0b37"
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

run "release_manifest_ami_is_plan_time_generation_input" {
  command = plan

  variables {
    controlplane_image             = null
    worker_ami_id                  = null
    release_artifacts_manifest_url = "https://releases.example.test/aws-artifacts.json"
    create_worker                  = true
  }

  assert {
    condition = (
      local.worker_ami_id == "ami-00000000000000000" &&
      length(module.worker_group) == 2 &&
      contains(keys(module.worker_group), local.worker_pool_names.run) &&
      contains(keys(module.worker_group), local.worker_pool_names.build)
    )
    error_message = "The real release-artifacts module must resolve the manifest AMI while Worker generation keys are still plan-time known."
  }
}

run "derived_guest_disk_changes_generation" {
  command = plan

  variables {
    worker_artifact_cache_max_mib = 8192
  }

  assert {
    condition = (
      local.worker_pool_names.run != "run-a3e8694369204dabbbb71776cc00e4493a8481fd6578d0155cf6641477598aac" &&
      local.worker_pool_names.build == "build-b434d01fbfaf692968c80748167e3c2856f65e7d2e373241dbf5da55350d0b37"
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
      local.worker_pool_names.run == "run-a3e8694369204dabbbb71776cc00e4493a8481fd6578d0155cf6641477598aac" &&
      local.worker_pool_names.build != "build-b434d01fbfaf692968c80748167e3c2856f65e7d2e373241dbf5da55350d0b37"
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
      local.worker_pool_names.run == "run-a3e8694369204dabbbb71776cc00e4493a8481fd6578d0155cf6641477598aac" &&
      local.worker_pool_names.build != "build-b434d01fbfaf692968c80748167e3c2856f65e7d2e373241dbf5da55350d0b37"
    )
    error_message = "Changed effective build per-VM overrides must create a new build Pool generation without rotating the run Pool."
  }
}

run "cache_policy_changes_provider_generation" {
  command = plan

  variables {
    worker_substrate_cache_max_mib = 24576
    worker_artifact_cache_max_mib  = 24576
  }

  assert {
    condition = (
      local.worker_pool_names.run != "run-a3e8694369204dabbbb71776cc00e4493a8481fd6578d0155cf6641477598aac" &&
      local.worker_pool_names.build == "build-b434d01fbfaf692968c80748167e3c2856f65e7d2e373241dbf5da55350d0b37"
    )
    error_message = "Changing immutable run cache policy must rotate its provider generation even when aggregate guest capacity is unchanged."
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
      local.worker_pool_names.run == "run-a3e8694369204dabbbb71776cc00e4493a8481fd6578d0155cf6641477598aac" &&
      local.worker_pool_names.build == "build-b434d01fbfaf692968c80748167e3c2856f65e7d2e373241dbf5da55350d0b37"
    )
    error_message = "Mutable ASG minimum and maximum sizes must not rotate Worker Pool generations."
  }
}

run "retained_generation_coexists_at_scale_zero" {
  command = plan

  variables {
    create_worker                     = true
    worker_network_blocked_ipv4_cidrs = ["10.0.0.0/8", "169.254.0.0/16", "192.0.2.0/24"]
    worker_launch_timeout_seconds     = 1200
    retained_worker_generations = {
      "run-12afa0d4d9e670707fc37a41731c502cd3b52bb7ca50f7f25621b7330914ae89" = {
        generation_inputs = {
          ami_id                = "ami-11111111111111111"
          instance_type         = "c8i.xlarge"
          nested_virtualization = true
          roles                 = ["run"]
          supply = {
            contract_digest     = "4cb5813282fb885829c3bcf1ccd3305b24a97fc80937c92f417171a12fce5d6a"
            enable_ssm          = false
            build_policy_digest = null
            network = {
              blocked_ipv4_cidrs = ["10.0.0.0/8", "169.254.0.0/16", "192.0.2.0/24"]
              link_pool          = "169.254.64.0/18"
              resolver_ipv4      = "10.81.0.2"
              translation_pool   = "100.96.0.0/16"
            }
            platform_store = {
              uri         = "s3://helmr-test-platform/objects"
              bucket_arn  = "arn:aws:s3:::helmr-test-platform"
              kms_key_arn = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
            }
            image_cache = null
            root_volume = {
              size_gb    = 120
              iops       = 3000
              throughput = 125
            }
            disk = {
              total_mib           = 262144
              reserve_mib         = 1024
              substrate_cache_mib = 32768
              artifact_cache_mib  = 16384
              build_cache_mib     = null
              build_scratch_mib   = null
            }
            lifecycle = {
              health_check_grace_period_seconds               = 900
              launch_lifecycle_heartbeat_timeout_seconds      = 777
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
            cpu_millis               = 8000
            memory_mib               = 16384
            guest_ephemeral_disk_mib = 212992
            vm_slots                 = 4
            build_executors          = 0
          }
          per_vm = {
            cpu_millis               = 2000
            memory_mib               = 4096
            guest_ephemeral_disk_mib = 32768
          }
        }
        min_size = 0
        max_size = 3
        sealed_provider_definition = {
          user_data_base64                                = "cmV0YWluZWQtdXNlci1kYXRh"
          permission_policy_json                          = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
          boundary_policy_json                            = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
          enable_ssm                                      = false
          launch_template_version                         = "7"
          health_check_grace_period_seconds               = 900
          launch_lifecycle_heartbeat_timeout_seconds      = 777
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
    }
  }

  assert {
    condition = (
      length(local.worker_generations) == 3 &&
      one([for generation in values(local.worker_generations) : generation if generation.ami_id == "ami-11111111111111111"]).min_size == 0 &&
      one([for generation in values(local.worker_generations) : generation if generation.ami_id == "ami-11111111111111111"]).max_size == 3 &&
      one([for generation in values(local.worker_generations) : generation if generation.ami_id == "ami-11111111111111111"]).provider_name != local.worker_generations[local.worker_pool_names.run].provider_name &&
      one([for pool_name, generation in local.worker_generations : pool_name == format("run-%s", sha256(jsonencode(generation.generation_inputs))) if generation.ami_id == "ami-11111111111111111"])
    )
    error_message = "A retained immutable Pool must coexist with both current generations as a distinct scale-zero provider binding."
  }


  assert {
    condition = (
      length(module.worker_group) == 3 &&
      length([for generation in values(local.worker_generations) : generation if generation.ami_id == "ami-11111111111111111"]) == 1 &&
      contains(keys(module.worker_group), local.worker_pool_names.run) &&
      contains(keys(module.worker_group), local.worker_pool_names.build)
    )
    error_message = "Terraform must materialize one independently addressable provider generation per current or retained Product Pool."
  }

  assert {
    condition = (
      one([for pool_name, generation in local.worker_generations : module.worker_group[pool_name] if generation.ami_id == "ami-11111111111111111"]).launch_template_version == "7" &&
      one([for pool_name, generation in local.worker_generations : module.worker_group[pool_name] if generation.ami_id == "ami-11111111111111111"]).sealed_provider_definition.user_data_base64 == base64encode("retained-user-data") &&
      one([for pool_name, generation in local.worker_generations : module.worker_group[pool_name] if generation.ami_id == "ami-11111111111111111"]).sealed_provider_definition.enable_ssm == false &&
      one([for pool_name, generation in local.worker_generations : module.worker_group[pool_name] if generation.ami_id == "ami-11111111111111111"]).sealed_provider_definition.launch_lifecycle_heartbeat_timeout_seconds == 777
    )
    error_message = "A retained Pool must remain pinned to its exact realized provider authority and launch-template version when the current launch policy changes."
  }
}
