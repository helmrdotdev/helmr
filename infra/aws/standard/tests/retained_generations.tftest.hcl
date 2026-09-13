mock_provider "aws" {
  mock_data "aws_region" { defaults = { region = "us-east-1" } }
  mock_data "aws_caller_identity" { defaults = { account_id = "111122223333" } }
  mock_data "aws_partition" { defaults = { partition = "aws", dns_suffix = "amazonaws.com" } }
  mock_data "aws_vpc" { defaults = { cidr_block = "10.91.0.0/16" } }
  mock_data "aws_iam_policy" {
    defaults = {
      policy = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"ExternalWorkerCeiling\",\"Effect\":\"Allow\",\"Action\":[\"s3:GetObject\"],\"Resource\":\"*\"}]}"
    }
  }
  mock_resource "aws_secretsmanager_secret" { defaults = { arn = "arn:aws:secretsmanager:us-east-1:111122223333:secret:mock" } }
  mock_resource "aws_s3_bucket" { defaults = { arn = "arn:aws:s3:::mock-bucket" } }
  mock_resource "aws_kms_key" { defaults = { arn = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000" } }
  mock_resource "aws_iam_role" { defaults = { arn = "arn:aws:iam::111122223333:role/mock" } }
  mock_resource "aws_iam_policy" { defaults = { arn = "arn:aws:iam::111122223333:policy/mock" } }
  mock_resource "aws_launch_template" { defaults = { id = "lt-0123456789abcdef0", latest_version = 1 } }
  mock_resource "aws_lb" { defaults = { arn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/app/mock/0000000000000000" } }
  mock_resource "aws_lb_target_group" { defaults = { arn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/mock/0000000000000000" } }
  mock_resource "aws_elasticache_replication_group" { defaults = { primary_endpoint_address = "redis.example.test" } }
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
  aws_region                          = "us-east-1"
  worker_group_name                   = "workers"
  region_id                           = "us-east-1"
  platform_store_uri                  = "s3://helmr-test-platform/objects"
  platform_store_bucket_arn           = "arn:aws:s3:::helmr-test-platform"
  platform_store_kms_key_arn          = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
  clickhouse_url                      = "https://clickhouse.example.test:8443"
  helmr_version                       = "v0.0.0-test"
  github_oauth_client_id              = "github-client"
  worker_network_blocked_ipv4_cidrs   = ["10.0.0.0/8", "10.91.0.0/16", "169.254.0.0/16"]
  public_url                          = "https://helmr.example.test"
  certificate_arn                     = "arn:aws:acm:us-east-1:111122223333:certificate/00000000-0000-0000-0000-000000000000"
  controlplane_image                  = "111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  worker_ami_id                       = "ami-00000000000000000"
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
}

run "legacy_retained_generation_key_is_preserved" {
  command = plan

  variables {
    retained_worker_generations = {
      "execution-c4938326fca64d4a93c411cdec03dca85ee76eca0f4ad47a44461625be8abb43" = {
        generation_inputs = {
          ami_id                = "ami-00000000000000000"
          instance_type         = "c8i.xlarge"
          nested_virtualization = true
          supply = {
            contract_digest = "precandidate-worker-contract-digest"
            enable_ssm      = true
            network = {
              blocked_ipv4_cidrs = ["10.0.0.0/8", "10.91.0.0/16", "169.254.0.0/16"]
              link_pool          = "169.254.64.0/18"
              resolver_ipv4      = "10.91.0.2"
              translation_pool   = "100.96.0.0/16"
            }
            platform_store = {
              uri         = "s3://helmr-test-platform/objects"
              bucket_arn  = "arn:aws:s3:::helmr-test-platform"
              kms_key_arn = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
            }
            root_volume = {
              size_gb    = 120
              iops       = 6000
              throughput = 250
            }
            disk = {
              total_mib           = 262144
              reserve_mib         = 1024
              substrate_cache_mib = 32768
              artifact_cache_mib  = 16384
            }
            lifecycle = {
              health_check_grace_period_seconds               = 900
              launch_lifecycle_heartbeat_timeout_seconds      = 900
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
          }
          per_vm = {
            cpu_millis               = 2000
            memory_mib               = 4096
            guest_ephemeral_disk_mib = 32768
          }
        }
        min_size = 0
        max_size = 1
        sealed_provider_definition = {
          user_data_base64                                = "dGVzdA=="
          permission_policy_json                          = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
          boundary_policy_json                            = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
          enable_ssm                                      = true
          launch_template_version                         = "1"
          health_check_grace_period_seconds               = 900
          launch_lifecycle_heartbeat_timeout_seconds      = 900
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
      one(keys(var.retained_worker_generations)) == "execution-${sha256(jsonencode(one(values(var.retained_worker_generations)).generation_inputs))}" &&
      contains(keys(output.worker_generation_definitions), one(keys(var.retained_worker_generations)))
    )
    error_message = "legacy retained generation keys must remain valid without supply-side external boundary fields"
  }
}

run "retained_external_generation_is_sealed_only" {
  command = plan

  variables {
    create_worker                     = true
    external_permissions_boundary_arn = "arn:aws:iam::111122223333:policy/current-worker-boundary"
    retained_worker_generations = {
      "execution-c4938326fca64d4a93c411cdec03dca85ee76eca0f4ad47a44461625be8abb43" = {
        generation_inputs = {
          ami_id                = "ami-00000000000000000"
          instance_type         = "c8i.xlarge"
          nested_virtualization = true
          supply = {
            contract_digest = "precandidate-worker-contract-digest"
            enable_ssm      = true
            network = {
              blocked_ipv4_cidrs = ["10.0.0.0/8", "10.91.0.0/16", "169.254.0.0/16"]
              link_pool          = "169.254.64.0/18"
              resolver_ipv4      = "10.91.0.2"
              translation_pool   = "100.96.0.0/16"
            }
            platform_store = {
              uri         = "s3://helmr-test-platform/objects"
              bucket_arn  = "arn:aws:s3:::helmr-test-platform"
              kms_key_arn = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
            }
            root_volume = {
              size_gb    = 120
              iops       = 6000
              throughput = 250
            }
            disk = {
              total_mib           = 262144
              reserve_mib         = 1024
              substrate_cache_mib = 32768
              artifact_cache_mib  = 16384
            }
            lifecycle = {
              health_check_grace_period_seconds               = 900
              launch_lifecycle_heartbeat_timeout_seconds      = 900
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
          }
          per_vm = {
            cpu_millis               = 2000
            memory_mib               = 4096
            guest_ephemeral_disk_mib = 32768
          }
        }
        min_size = 0
        max_size = 1
        sealed_provider_definition = {
          user_data_base64                                = "dGVzdA=="
          permission_policy_json                          = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
          boundary_policy_json                            = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"ExternalWorkerCeiling\",\"Effect\":\"Allow\",\"Action\":[\"s3:GetObject\"],\"Resource\":\"*\"}]}"
          boundary_policy_arn                             = "arn:aws:iam::111122223333:policy/org-worker-boundary"
          enable_ssm                                      = true
          launch_template_version                         = "1"
          health_check_grace_period_seconds               = 900
          launch_lifecycle_heartbeat_timeout_seconds      = 900
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
      length(module.worker_group) == 2 &&
      module.worker_group[one(keys(var.retained_worker_generations))].iam_role_name != null &&
      module.worker_group[local.worker_pool_name].sealed_provider_definition.boundary_policy_arn == var.external_permissions_boundary_arn &&
      module.worker_group[one(keys(var.retained_worker_generations))].sealed_provider_definition.boundary_policy_arn == "arn:aws:iam::111122223333:policy/org-worker-boundary"
    )
    error_message = "current generation must seal stack external_permissions_boundary_arn while retained external generations keep the sealed org boundary"
  }
}
