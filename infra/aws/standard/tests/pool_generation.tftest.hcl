mock_provider "aws" {
  mock_data "aws_region" { defaults = { region = "us-east-1" } }
  mock_data "aws_caller_identity" { defaults = { account_id = "111122223333" } }
  mock_data "aws_partition" { defaults = { partition = "aws", dns_suffix = "amazonaws.com" } }
  mock_data "aws_vpc" { defaults = { cidr_block = "10.91.0.0/16" } }
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
  clickhouse_url                    = "https://clickhouse.example.test:8443"
  helmr_version                     = "v0.0.0-test"
  github_oauth_client_id            = "github-client"
  worker_network_blocked_ipv4_cidrs = ["10.0.0.0/8", "10.91.0.0/16", "169.254.0.0/16"]
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
}

run "baseline_execution_generation" {
  command = plan

  assert {
    condition = (
      local.worker_pool_name == "execution-${sha256(jsonencode(local.worker_generation_inputs))}" &&
      local.worker_pool_name == "execution-0b880387b51901f69d0b462c9f9c53eeed480c07c5a16a60cfbe86bd5de40e9f" &&
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
  variables { worker_vm_scratch_disk_mib = 40960 }

  assert {
    condition     = local.worker_pool_name == "execution-efe29d072e511593fa9a03d6f42b72074f721083b4986789aa00e871cd91377f"
    error_message = "an immutable execution-shape change must rotate the Pool generation"
  }
}

run "scale_policy_does_not_rotate_generation" {
  command = plan
  variables {
    worker_min_size = 5
    worker_max_size = 9
  }

  assert {
    condition = (
      local.worker_pool_name == "execution-0b880387b51901f69d0b462c9f9c53eeed480c07c5a16a60cfbe86bd5de40e9f" &&
      one(values(output.worker_generation_definitions)).min_size == 5 &&
      one(values(output.worker_generation_definitions)).max_size == 9
    )
    error_message = "mutable ASG size must not rotate the execution Pool generation"
  }
}
