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

  mock_resource "aws_launch_template" {
    defaults = { id = "lt-00000000000000000" }
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

run "execution_worker_is_immutable_and_launch_gated" {
  command = apply

  variables {
    launch_lifecycle_heartbeat_timeout_seconds = 321
  }

  assert {
    condition = (
      aws_autoscaling_group.worker.protect_from_scale_in &&
      aws_autoscaling_group.worker.launch_template[0].id == aws_launch_template.worker.id &&
      aws_autoscaling_group.worker.launch_template[0].version == tostring(aws_launch_template.worker.latest_version) &&
      aws_launch_template.worker.image_id == var.ami_id &&
      aws_launch_template.worker.metadata_options[0].http_tokens == "required" &&
      aws_launch_template.worker.metadata_options[0].http_put_response_hop_limit == 1
    )
    error_message = "execution capacity must pin its AMI and launch-template generation behind protected IMDSv2-only instances"
  }

  assert {
    condition = (
      length(aws_autoscaling_group.worker.initial_lifecycle_hook) == 2 &&
      one([for hook in aws_autoscaling_group.worker.initial_lifecycle_hook : hook if hook.lifecycle_transition == "autoscaling:EC2_INSTANCE_LAUNCHING"]).default_result == "ABANDON" &&
      one([for hook in aws_autoscaling_group.worker.initial_lifecycle_hook : hook if hook.lifecycle_transition == "autoscaling:EC2_INSTANCE_TERMINATING"]).default_result == "CONTINUE"
    )
    error_message = "execution workers must qualify before launch admission and drain during termination"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "/usr/local/sbin/helmr-prepare-root '128849018880'") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "ExecStart=/usr/local/bin/worker") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_POOL_NAME=execution-v1") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "CPU_TEMPLATE_HELPER_PATH=/usr/local/bin/cpu-template-helper") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_NETWORK_RESOLVER_IPV4=10.20.0.2") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "drain-complete")
    )
    error_message = "execution worker bootstrap must carry the exact runtime, network, Pool, and lifecycle contract"
  }

  assert {
    condition = (
      strcontains(aws_iam_role_policy.worker.policy, "${var.platform_store_bucket_arn}/objects/sha256/*") &&
      strcontains(aws_iam_role_policy.worker.policy, var.platform_store_kms_key_arn) &&
      !strcontains(aws_iam_role_policy.worker.policy, "CreatePlatformObjects") &&
      !strcontains(aws_iam_role_policy.worker.policy, "EncryptPlatformObjects")
    )
    error_message = "execution workers must have read-only Platform Artifact authority"
  }
}

run "worker_without_ssm_has_exact_permission_boundary" {
  command = plan

  variables { enable_ssm = false }

  assert {
    condition = (
      aws_iam_role.worker.permissions_boundary == aws_iam_policy.worker_boundary.arn &&
      jsondecode(aws_iam_policy.worker_boundary.policy) == jsondecode(aws_iam_role_policy.worker.policy)
    )
    error_message = "workers without SSM must retain a boundary exactly equal to their permissions"
  }
}

run "explicit_disk_must_exceed_reserve" {
  command = plan
  variables {
    worker_disk_mib         = 1024
    worker_disk_reserve_mib = 1024
  }
  expect_failures = [terraform_data.network_preconditions]
}

run "generated_environment_key_is_reserved" {
  command = plan
  variables {
    worker_environment = { AWS_REGION = "us-west-2" }
  }
  expect_failures = [terraform_data.network_preconditions]
}

run "worker_pool_name_is_reserved" {
  command = plan
  variables {
    worker_environment = { WORKER_POOL_NAME = "other" }
  }
  expect_failures = [terraform_data.network_preconditions]
}

run "additional_worker_environment_is_rendered" {
  command = plan
  variables {
    worker_environment = { HELMR_TEST_FLAG = "enabled" }
  }
  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_TEST_FLAG=enabled") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "MKFS_EXT4_PATH=/usr/local/libexec/helmr/mkfs.ext4") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "MKE2FS_CONFIG_PATH=/usr/share/helmr/mke2fs.conf")
    )
    error_message = "non-reserved operator environment must be rendered"
  }
}
