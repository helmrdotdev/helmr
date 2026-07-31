mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition = "aws"
    }
  }

  mock_data "aws_vpc" {
    defaults = {
      cidr_block = "10.20.0.0/16"
    }
  }

  mock_resource "aws_launch_template" {
    defaults = {
      id = "lt-00000000000000000"
    }
  }
}

variables {
  name                       = "helmr-test-run"
  worker_group_id            = "run-workers"
  worker_roles               = ["run"]
  network_blocked_ipv4_cidrs = ["10.0.0.0/8", "169.254.0.0/16"]
  network_link_pool          = "169.254.64.0/18"
  network_translation_pool   = "100.96.0.0/16"
  vpc_id                     = "vpc-00000000000000000"
  subnet_ids                 = ["subnet-00000000000000000"]
  ami_id                     = "ami-00000000000000000"
  worker_control_url         = "https://control.example.test"
  cas_uri                    = "s3://helmr-test-cas"
  cas_bucket_arn             = "arn:aws:s3:::helmr-test-cas"
  kms_key_arn                = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
  platform_store_uri         = "s3://helmr-test-runtime/objects"
  platform_store_bucket_arn  = "arn:aws:s3:::helmr-test-runtime"
  platform_store_kms_key_arn = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
  build_policy_digest        = null
  min_size                   = 0
  max_size                   = 1
  secret_arns = {
    checkpoint_encryption_key = "arn:aws:secretsmanager:us-east-1:111122223333:secret:checkpoint"
  }
}

run "controller_owns_protected_capacity" {
  command = plan

  variables {
    launch_lifecycle_heartbeat_timeout_seconds = 321
  }

  assert {
    condition     = aws_autoscaling_group.worker.protect_from_scale_in
    error_message = "managed controller capacity must start protected from scale in"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "ExecStart=helmr-worker") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "nameserver 10.20.0.2") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS=[\"10.0.0.0/8\",\"169.254.0.0/16\"]") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_WORKER_NETWORK_LINK_POOL=169.254.64.0/18") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_WORKER_NETWORK_RESOLVER_IPV4=10.20.0.2") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_WORKER_NETWORK_TRANSLATION_POOL=100.96.0.0/16")
    )
    error_message = "worker user data must receive the complete generic routed-network configuration"
  }

  assert {
    condition     = strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_WORKER_DISK_RESERVE_MIB=1024")
    error_message = "worker user data must pin the disk reserve used by certified capacity math"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_PLATFORM_STORE_URI=s3://helmr-test-runtime/objects") &&
      strcontains(aws_iam_role_policy.worker.policy, "${var.platform_store_bucket_arn}/objects/sha256/*") &&
      strcontains(aws_iam_role_policy.worker.policy, var.platform_store_kms_key_arn) &&
      !strcontains(aws_iam_role_policy.worker.policy, "CreatePlatformObjects") &&
      !strcontains(aws_iam_role_policy.worker.policy, "EncryptPlatformObjects")
    )
    error_message = "run-only workers must receive read-only Platform Artifact authority"
  }

  assert {
    condition     = !strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_BUILD_POLICY_PATH") && !strcontains(base64decode(aws_launch_template.worker.user_data), "release install")
    error_message = "run-only workers must not receive or install the current build policy"
  }

  assert {
    condition = (
      !strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_WORKER_BUILD_CACHE_DIR") &&
      !strcontains(base64decode(aws_launch_template.worker.user_data), "build-cache.ext4") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "20-run-only.conf")
    )
    error_message = "run-only workers must not receive build filesystems and must remove the image's BuildKit dependency"
  }

  assert {
    condition     = strcontains(base64decode(aws_launch_template.worker.user_data), "launch_timeout='321'") && strcontains(base64decode(aws_launch_template.worker.user_data), "drain-complete") && strcontains(base64decode(aws_launch_template.worker.user_data), "ABANDON")
    error_message = "worker lifecycle handling must bound launch readiness and bypass repeated drain after durable local completion"
  }
}

run "build_worker_installs_exact_policy_before_service" {
  command = plan

  variables {
    name                       = "helmr-test-build"
    worker_group_id            = "build-workers"
    worker_roles               = ["build"]
    worker_capacity_vcpus      = 4
    worker_capacity_memory_mib = 8192
    worker_execution_slots     = 1
    build_policy_digest        = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    build_cache_mib            = 8192
    build_scratch_mib          = 34816
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_BUILD_POLICY_PATH=/etc/helmr/build-policy.json") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "\"$worker_binary\" release install") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "--store 's3://helmr-test-runtime/objects'") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "--digest 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "--output /etc/helmr/build-policy.json")
    )
    error_message = "build-capable worker bootstrap must install the exact policy through the worker CLI"
  }

  assert {
    condition = (
      strcontains(aws_iam_role_policy.worker.policy, "${var.platform_store_bucket_arn}/objects/sha256/*") &&
      strcontains(aws_iam_role_policy.worker.policy, "CreatePlatformObjects") &&
      strcontains(aws_iam_role_policy.worker.policy, "EncryptPlatformObjects") &&
      !strcontains(aws_iam_role_policy.worker.policy, "${var.platform_store_bucket_arn}/control/runtime")
    )
    error_message = "build worker IAM must publish immutable Platform candidates without rollout lineage authority"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "build-cache.ext4") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "build-scratch.ext4") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "mkfs.ext4 -F -q -m 0") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "Options=loop,nosuid,nodev,nodiscard") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "--root /var/lib/helmr/cache/buildkit") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_WORKER_WORK_DIR=/var/lib/helmr/scratch/worker") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "HELMR_WORKER_FIRECRACKER_CHROOT_DIR=/var/lib/helmr/scratch/jailer")
    )
    error_message = "build workers must mount distinct fixed ext4 filesystems and keep BuildKit on cache while build jail and worker roots stay on scratch"
  }

  assert {
    condition     = !strcontains(aws_iam_role_policy.worker.policy, "RetainedArtifacts") && !strcontains(jsonencode(aws_iam_role.worker.tags), "RetainedCASPublisher")
    error_message = "workers must not receive retained artifact authority before the producer is connected"
  }
}

run "build_worker_requires_policy_digest" {
  command = plan

  variables {
    worker_roles               = ["build"]
    worker_capacity_vcpus      = 4
    worker_capacity_memory_mib = 8192
    build_policy_digest        = null
  }

  expect_failures = [terraform_data.network_preconditions]
}

run "build_worker_rejects_scratch_below_large_vm_boundary" {
  command = plan

  variables {
    name                       = "helmr-test-build"
    worker_group_id            = "build-workers"
    worker_roles               = ["build"]
    worker_capacity_vcpus      = 4
    worker_capacity_memory_mib = 8192
    worker_execution_slots     = 1
    vm_scratch_disk_mib        = 40000
    build_policy_digest        = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    build_cache_mib            = 8192
    build_scratch_mib          = 42047
  }

  expect_failures = [terraform_data.network_preconditions]
}

run "run_only_worker_rejects_current_policy" {
  command = plan

  variables {
    build_policy_digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }

  expect_failures = [terraform_data.network_preconditions]
}

run "controller_requires_lifecycle_hooks" {
  command = plan

  variables {
    enable_lifecycle_hooks = false
  }

  expect_failures = [aws_autoscaling_group.worker]
}

run "explicit_disk_must_exceed_reserve" {
  command = plan

  variables {
    worker_disk_mib         = 1024
    worker_disk_reserve_mib = 1024
  }

  expect_failures = [terraform_data.network_preconditions]
}
