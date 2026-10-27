mock_provider "aws" {
  mock_resource "aws_cloudwatch_event_rule" { defaults = { arn = "arn:aws:events:us-east-1:000000000000:rule/mock" } }
  mock_data "aws_availability_zones" { defaults = { names = ["us-east-1a", "us-east-1b"] } }
  mock_resource "aws_ecs_cluster" { defaults = { arn = "arn:aws:ecs:us-east-1:111122223333:cluster/test" } }
  mock_resource "aws_service_discovery_service" { defaults = { arn = "arn:aws:servicediscovery:us-east-1:111122223333:service/srv-1234567890123456" } }
  mock_resource "aws_ecs_task_definition" { defaults = { arn = "arn:aws:ecs:us-east-1:111122223333:task-definition/test:1" } }
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

variables {
  audit_retention_seconds = 2592000
  controlplane_environment = {
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
  worker_network_blocked_ipv4_cidrs        = ["10.90.0.0/16", "10.91.0.0/16", "10.92.0.0/16", "169.254.0.0/16"]
  app_hostname                             = "helmr.example.test"
  company_client_ipv4_cidrs                = ["10.92.0.0/16"]
  clickhouse_security_group_id             = "sg-0123456789abcdef0"
  clickhouse_port                          = 8443
  controlplane_image_repository_arn        = "arn:aws:ecr:us-east-1:111122223333:repository/helmr"
  proxy_image                              = "111122223333.dkr.ecr.us-east-1.amazonaws.com/squid@sha256:8a3baed477e2c282ab8aa5edad442f69873246964f225c5c2ae8364b6610963c"
  proxy_image_repository_arn               = "arn:aws:ecr:us-east-1:111122223333:repository/squid"
  certificate_arn                          = "arn:aws:acm:us-east-1:111122223333:certificate/00000000-0000-0000-0000-000000000000"
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


run "private_network_bootstrap" {
  command = apply
  assert {
    condition     = toset([for association in aws_route53_zone.application.vpc : association.vpc_id]) == toset([output.controlplane_vpc_id, output.execution_vpc_id])
    error_message = "The application zone must initially associate exactly the Control Plane and execution VPCs."
  }
  assert {
    condition     = length(output.execution_private_subnet_ids) == 2
    error_message = "Execution bootstrap must expose both private subnets."
  }
}

run "private_baseline" {
  command = apply
  variables {
    create_controlplane_service = true
    create_worker               = true
  }
  assert {
    condition     = output.controlplane_url == "https://helmr.example.test" && output.worker_controlplane_url == output.controlplane_url && output.app_origin == output.controlplane_url
    error_message = "Private callers and browser links must share the internal HTTPS origin."
  }
  assert {
    condition     = length(aws_vpc_endpoint.platform) == 4 && aws_route53_resolver_firewall_config.platform.firewall_fail_open == "DISABLED"
    error_message = "Private bootstrap endpoints and fail-closed resolver policy are required."
  }
  assert {
    condition     = !aws_ecs_service.proxy[0].network_configuration[0].assign_public_ip && length(local.external_domains) == 6
    error_message = "Proxy must use private addresses and only configured integration destinations."
  }
  assert {
    condition = (
      aws_route53_resolver_firewall_domain_list.all.domains == toset(["*."]) &&
      alltrue([for domain in aws_route53_resolver_firewall_domain_list.allowed.domains : endswith(domain, ".")])
    )
    error_message = "DNS firewall domains must use the fully qualified names returned by the resolver API."
  }
  assert {
    condition     = aws_service_discovery_service.proxy.health_check_custom_config[0].failure_threshold == 1
    error_message = "Proxy discovery must explicitly enable the custom health checks managed by ECS."
  }
  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.proxy.container_definitions)[0].linuxParameters.capabilities.add == [] &&
      jsondecode(aws_ecs_task_definition.proxy.container_definitions)[0].linuxParameters.capabilities.drop == ["ALL"]
    )
    error_message = "The proxy must explicitly add no capabilities while dropping every default capability."
  }
}

run "company_application_dns" {
  command = plan
  variables {
    additional_application_vpc_ids = ["vpc-0123456789abcdef0", "vpc-12345678"]
  }
  assert {
    condition = toset([for association in aws_route53_zone.application.vpc : association.vpc_id]) == toset(concat(
      [output.controlplane_vpc_id, output.execution_vpc_id], var.additional_application_vpc_ids
    ))
    error_message = "The application zone must associate exactly its two platform VPCs and the supplied company VPCs."
  }
}

run "application_dns_rejects_invalid_vpc_id" {
  command = plan
  variables { additional_application_vpc_ids = ["subnet-0123456789abcdef0"] }
  expect_failures = [var.additional_application_vpc_ids]
}

run "application_dns_rejects_unnormalized_vpc_id" {
  command = plan
  variables { additional_application_vpc_ids = [" vpc-0123456789abcdef0 "] }
  expect_failures = [var.additional_application_vpc_ids]
}

run "disabled_slack_omits_destinations" {
  command = plan
  variables { slack_transport = "disabled" }
  assert {
    condition     = !contains(local.external_domains, "slack.com") && !contains(local.external_domains, "wss-primary.slack.com")
    error_message = "Disabled Slack must not retain its proxy/DNS allowance."
  }
}

run "guest_management_denies_required" {
  command = plan
  variables { worker_network_blocked_ipv4_cidrs = ["10.91.0.0/16"] }
  expect_failures = [terraform_data.private_contract]
}

run "http_delivery_rejected" {
  command = plan
  variables { slack_transport = "http" }
  expect_failures = [var.slack_transport]
}

run "retained_generation_requires_new_company_denies" {
  command = plan
  variables {
    create_worker                     = true
    company_client_ipv4_cidrs         = ["10.92.0.0/16", "8.8.0.0/16"]
    worker_network_blocked_ipv4_cidrs = ["10.90.0.0/16", "10.91.0.0/16", "10.92.0.0/16", "8.8.0.0/16", "169.254.0.0/16"]
    retained_worker_generations       = run.private_baseline.worker_generation_definitions
  }
  expect_failures = [terraform_data.private_contract, terraform_data.network_installation_contract]
}
