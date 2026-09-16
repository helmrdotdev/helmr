mock_provider "aws" {
  mock_data "aws_region" {
    defaults = { region = "us-east-1" }
  }

  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }

  mock_data "aws_vpc" {
    defaults = { cidr_block = "10.20.0.0/16" }
  }

  mock_data "aws_caller_identity" {
    defaults = { account_id = "111122223333" }
  }

  mock_data "aws_iam_policy" {
    defaults = {
      policy = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"ExternalWorkerCeiling\",\"Effect\":\"Allow\",\"Action\":[\"s3:GetObject\"],\"Resource\":\"*\"}]}"
    }
  }

  mock_resource "aws_launch_template" {
    defaults = { id = "lt-00000000000000000", latest_version = 1 }
  }

  mock_resource "aws_iam_policy" {
    defaults = { arn = "arn:aws:iam::111122223333:policy/helmr-test-worker-boundary" }
  }
}

variables {
  name                       = "helmr-test-worker"
  worker_pool_name           = "execution-v1"
  network_blocked_ipv4_cidrs = ["10.0.0.0/8", "169.254.0.0/16"]
  network_link_pool          = "169.254.64.0/18"
  network_translation_pool   = "100.96.0.0/16"
  vpc_id                     = "vpc-00000000000000000"
  subnet_ids                 = ["subnet-00000000000000000"]
  ami_id                     = "ami-00000000000000000"
  worker_controlplane_url    = "https://controlplane.example.test"
  cas_uri                    = "s3://helmr-test-cas"
  cas_bucket_arn             = "arn:aws:s3:::helmr-test-cas"
  kms_key_arn                = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
  platform_store_uri         = "s3://helmr-test-runtime/objects"
  platform_store_bucket_arn  = "arn:aws:s3:::helmr-test-runtime"
  platform_store_kms_key_arn = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
  min_size                   = 0
  max_size                   = 1
  root_volume_size_gb        = 120
  secret_arns = {
    checkpoint_encryption_key = "arn:aws:secretsmanager:us-east-1:111122223333:secret:checkpoint"
    worker_enrollment_token   = "arn:aws:secretsmanager:us-east-1:111122223333:secret:worker-enrollment"
  }
}

run "default_export" {
  command = apply

  variables { enable_ssm = false }

  assert {
    condition = (
      output.sealed_provider_definition.boundary_policy_arn == null &&
      length(aws_iam_policy.worker_boundary) == 1
    )
    error_message = "module-managed generations must seal a null external boundary discriminator"
  }
}

run "default_retain" {
  command = plan

  variables {
    enable_ssm                 = false
    sealed_provider_definition = run.default_export.sealed_provider_definition
  }

  assert {
    condition = (
      length(aws_iam_policy.worker_boundary) == 1 &&
      aws_iam_role.worker.permissions_boundary == one(aws_iam_policy.worker_boundary[*].arn)
    )
    error_message = "retaining the default sealed output must keep the module-managed boundary resource"
  }
}

run "external_boundary_is_not_module_managed" {
  command = plan

  variables {
    permissions_boundary_arn = "arn:aws:iam::111122223333:policy/org-worker-boundary"
  }

  assert {
    condition = (
      length(aws_iam_policy.worker_boundary) == 0 &&
      aws_iam_role.worker.permissions_boundary == var.permissions_boundary_arn &&
      output.sealed_provider_definition.boundary_policy_arn == var.permissions_boundary_arn &&
      jsondecode(output.sealed_provider_definition.boundary_policy_json).Statement[0].Sid == "ExternalWorkerCeiling" &&
      strcontains(aws_iam_role_policy.worker.policy, "${var.platform_store_bucket_arn}/objects/sha256/*")
    )
    error_message = "external boundaries must attach verified policy bytes without replacing the generated inline role policy"
  }
}

run "external_boundary_rejects_foreign_account" {
  command = plan

  variables {
    permissions_boundary_arn = "arn:aws:iam::999999999999:policy/org-worker-boundary"
  }

  expect_failures = [aws_iam_role.worker]
}

run "retained_external_boundary_rejects_input_switch" {
  command = plan

  variables {
    permissions_boundary_arn = "arn:aws:iam::111122223333:policy/other-worker-boundary"
    sealed_provider_definition = {
      user_data_base64                                = "dGVzdA=="
      permission_policy_json                          = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
      boundary_policy_json                            = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"ExternalWorkerCeiling\",\"Effect\":\"Allow\",\"Action\":[\"s3:GetObject\"],\"Resource\":\"*\"}]}"
      boundary_policy_arn                             = "arn:aws:iam::111122223333:policy/org-worker-boundary"
      enable_ssm                                      = false
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

  expect_failures = [aws_iam_role.worker]
}

run "retained_external_boundary_rejects_policy_drift" {
  command = plan

  variables {
    sealed_provider_definition = {
      user_data_base64                                = "dGVzdA=="
      permission_policy_json                          = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
      boundary_policy_json                            = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Sid\":\"StaleAuthority\",\"Effect\":\"Allow\",\"Action\":[\"s3:PutObject\"],\"Resource\":\"*\"}]}"
      boundary_policy_arn                             = "arn:aws:iam::111122223333:policy/org-worker-boundary"
      enable_ssm                                      = false
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

  expect_failures = [aws_iam_role.worker]
}

run "malformed_ceiling" {
  command = plan
  variables { permissions_boundary_arn = "workload" }
  expect_failures = [var.permissions_boundary_arn]
}

run "external_boundary_rejects_foreign_partition" {
  command = plan
  variables { permissions_boundary_arn = "arn:aws-us-gov:iam::111122223333:policy/workload" }
  expect_failures = [aws_iam_role.worker]
}
