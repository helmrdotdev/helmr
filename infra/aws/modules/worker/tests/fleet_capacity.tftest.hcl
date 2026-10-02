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
  computer_save_interval_seconds = 60
  computer_devices               = ["/dev/nbd0", "/dev/nbd1"]
  name                           = "helmr-test-worker"
  worker_pool_name               = "execution-v1"
  network_blocked_ipv4_cidrs     = ["10.0.0.0/8", "169.254.0.0/16"]
  network_link_pool              = "169.254.64.0/18"
  network_translation_pool       = "100.96.0.0/16"
  vpc_id                         = "vpc-00000000000000000"
  subnet_ids                     = ["subnet-00000000000000000"]
  ami_id                         = "ami-00000000000000000"
  worker_controlplane_url        = "https://controlplane.example.test"
  cas_uri                        = "s3://helmr-test-cas"
  cas_bucket_arn                 = "arn:aws:s3:::helmr-test-cas"
  kms_key_arn                    = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
  platform_store_uri             = "s3://helmr-test-runtime/objects"
  platform_store_bucket_arn      = "arn:aws:s3:::helmr-test-runtime"
  platform_store_kms_key_arn     = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
  min_size                       = 0
  max_size                       = 1
  root_volume_size_gb            = 120
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
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_COMPUTER_SAVE_EVERY=60s") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_COMPUTER_DEVICES=/dev/nbd0 /dev/nbd1") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "modprobe nbd nbds_max='2' max_part=0") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "test ! -e '/sys/block/nbd1/pid'") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "CPU_TEMPLATE_HELPER_PATH=/usr/local/bin/cpu-template-helper") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_NETWORK_RESOLVER_IPV4=10.20.0.2") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "drain-complete")
    )
    error_message = "execution worker bootstrap must carry the exact runtime, network, Pool, and lifecycle contract"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "EnvironmentFile=/etc/helmr/worker.env\nExecStart=/usr/local/bin/helmr-asg-lifecycle watch") &&
      !strcontains(base64decode(aws_launch_template.worker.user_data), "load_worker_env")
    )
    error_message = "lifecycle commands must inherit systemd-parsed environment values, including space-separated device paths"
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
      aws_iam_role.worker.permissions_boundary == aws_iam_policy.worker_boundary[0].arn &&
      jsondecode(aws_iam_policy.worker_boundary[0].policy) == jsondecode(aws_iam_role_policy.worker.policy)
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

run "reject_zero_save_interval" {
  command = plan
  variables { computer_save_interval_seconds = 0 }
  expect_failures = [var.computer_save_interval_seconds]
}
run "reject_duplicate_computer_device" {
  command = plan
  variables { computer_devices = ["/dev/nbd0", "/dev/nbd0"] }
  expect_failures = [var.computer_devices]
}

run "fixed_capacity_is_explicit_and_has_no_automatic_refresh" {
  command = plan
  variables {
    min_size         = 2
    max_size         = 2
    desired_capacity = 2
  }
  assert {
    condition     = aws_autoscaling_group.worker.desired_capacity == 2 && length(aws_autoscaling_group.worker.instance_refresh) == 0
    error_message = "Fixed supply must set desired capacity and never start an automatic refresh."
  }
}

run "external_capacity_owner" {
  command = plan
  assert {
    condition     = var.desired_capacity == null && length(aws_autoscaling_group.worker.instance_refresh) == 0
    error_message = "An external capacity owner must retain desired ownership without automatic refresh."
  }
}

run "desired_capacity_rejects_outside_bounds" {
  command = plan
  variables { desired_capacity = 2 }
  expect_failures = [aws_autoscaling_group.worker]
}

run "lifecycle_format_bound_0" {
  command = plan
  variables {
    vm_memory_mib        = 1
    vm_scratch_disk_mib  = 1
    computer_staging_mib = 1
  }
  assert {
    condition = (
      local.worker_slot_disk_bytes >= jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[0].bytes &&
      var.vm_memory_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[0].memory_mib &&
      var.vm_scratch_disk_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[0].scratch_mib &&
      var.computer_staging_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[0].staging_mib
    )
    error_message = "Infrastructure must bound the Worker's actual lifecycle format bytes."
  }
}

run "lifecycle_format_bound_1" {
  command = plan
  variables {
    vm_memory_mib        = 2048
    vm_scratch_disk_mib  = 32768
    computer_staging_mib = 65536
  }
  assert {
    condition = (
      local.worker_slot_disk_bytes >= jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[1].bytes &&
      var.vm_memory_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[1].memory_mib &&
      var.vm_scratch_disk_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[1].scratch_mib &&
      var.computer_staging_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[1].staging_mib
    )
    error_message = "Infrastructure must bound the Worker's actual lifecycle format bytes."
  }
}

run "lifecycle_format_bound_2" {
  command = plan
  variables {
    vm_memory_mib        = 4096
    vm_scratch_disk_mib  = 32768
    computer_staging_mib = 65536
  }
  assert {
    condition = (
      local.worker_slot_disk_bytes >= jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[2].bytes &&
      var.vm_memory_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[2].memory_mib &&
      var.vm_scratch_disk_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[2].scratch_mib &&
      var.computer_staging_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[2].staging_mib
    )
    error_message = "Infrastructure must bound the Worker's actual lifecycle format bytes."
  }
}

run "lifecycle_format_bound_3" {
  command = plan
  variables {
    vm_memory_mib        = 1048576
    vm_scratch_disk_mib  = 1048576
    computer_staging_mib = 65536
  }
  assert {
    condition = (
      local.worker_slot_disk_bytes >= jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[3].bytes &&
      var.vm_memory_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[3].memory_mib &&
      var.vm_scratch_disk_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[3].scratch_mib &&
      var.computer_staging_mib == jsondecode(file("../../../../internal/computerhost/testdata/host_disk_bounds.json"))[3].staging_mib
    )
    error_message = "Infrastructure must bound the Worker's actual lifecycle format bytes."
  }
}

run "reject_underfunded_lifecycle_slots" {
  command = plan
  variables {
    worker_disk_mib        = 131072
    worker_execution_slots = 2
    vm_scratch_disk_mib    = 32768
    vm_memory_mib          = 2048
    artifact_cache_max_mib = 16384
  }
  expect_failures = [terraform_data.network_preconditions]
}

run "funded_lifecycle_slots_propagate_staging" {
  command = plan
  variables {
    worker_disk_mib         = 491520
    worker_disk_reserve_mib = 8192
    worker_execution_slots  = 2
    vm_scratch_disk_mib     = 32768
    vm_memory_mib           = 2048
    artifact_cache_max_mib  = 16384
    computer_staging_mib    = 65536
  }
  assert {
    condition     = strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_COMPUTER_STAGING_MIB=65536")
    error_message = "Computer staging must reach the Worker as a module-owned setting."
  }
}

run "staging_environment_override_is_rejected" {
  command = plan
  variables { worker_environment = { WORKER_COMPUTER_STAGING_MIB = "1" } }
  expect_failures = [terraform_data.network_preconditions]
}
