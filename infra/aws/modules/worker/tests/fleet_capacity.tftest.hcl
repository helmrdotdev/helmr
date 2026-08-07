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

  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::111122223333:policy/helmr-test-worker-boundary"
    }
  }
}

variables {
  name                              = "helmr-test-run"
  worker_roles                      = ["run"]
  network_blocked_ipv4_cidrs        = ["10.0.0.0/8", "169.254.0.0/16"]
  network_link_pool                 = "169.254.64.0/18"
  network_translation_pool          = "100.96.0.0/16"
  vpc_id                            = "vpc-00000000000000000"
  subnet_ids                        = ["subnet-00000000000000000"]
  ami_id                            = "ami-00000000000000000"
  worker_controlplane_url           = "https://controlplane.example.test"
  cas_uri                           = "s3://helmr-test-cas"
  cas_bucket_arn                    = "arn:aws:s3:::helmr-test-cas"
  kms_key_arn                       = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
  platform_store_uri                = "s3://helmr-test-runtime/objects"
  platform_store_bucket_arn         = "arn:aws:s3:::helmr-test-runtime"
  platform_store_kms_key_arn        = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
  image_cache_registry_authority    = "111122223333.dkr.ecr.us-east-1.amazonaws.com"
  image_cache_repository_prefix     = "helmr-test/image-cache"
  image_cache_role_arn              = "arn:aws:iam::111122223333:role/helmr-test-image-cache"
  image_cache_repository_arn_prefix = "arn:aws:ecr:us-east-1:111122223333:repository/helmr-test/image-cache/"
  build_policy_digest               = null
  min_size                          = 0
  max_size                          = 1
  secret_arns = {
    checkpoint_encryption_key = "arn:aws:secretsmanager:us-east-1:111122223333:secret:checkpoint"
    worker_enrollment_token   = "arn:aws:secretsmanager:us-east-1:111122223333:secret:worker-enrollment"
  }
}

run "deployment_owns_protected_capacity" {
  command = plan

  variables {
    launch_lifecycle_heartbeat_timeout_seconds = 321
  }

  assert {
    condition     = aws_autoscaling_group.worker.protect_from_scale_in
    error_message = "deployment-owned capacity must start protected from scale in"
  }

  assert {
    condition = (
      aws_autoscaling_group.worker.instance_refresh[0].strategy == "Rolling" &&
      aws_autoscaling_group.worker.instance_refresh[0].preferences[0].min_healthy_percentage == 100 &&
      aws_autoscaling_group.worker.instance_refresh[0].preferences[0].max_healthy_percentage == 100 &&
      aws_autoscaling_group.worker.instance_refresh[0].preferences[0].scale_in_protected_instances == "Refresh" &&
      aws_autoscaling_group.worker.instance_refresh[0].preferences[0].standby_instances == "Terminate" &&
      aws_autoscaling_group.worker.instance_refresh[0].preferences[0].skip_matching
    )
    error_message = "launch-template changes must launch before termination and refresh protected and standby workers"
  }

  assert {
    condition = (
      toset(aws_autoscaling_group.worker.vpc_zone_identifier) == toset(var.subnet_ids) &&
      aws_autoscaling_group.worker.launch_template[0].id == aws_launch_template.worker.id &&
      aws_autoscaling_group.worker.launch_template[0].version == tostring(aws_launch_template.worker.latest_version) &&
      output.launch_template_id == aws_launch_template.worker.id &&
      output.launch_template_version == tostring(aws_launch_template.worker.latest_version)
    )
    error_message = "the worker ASG must use only the supplied Execution subnets and exact launch template version"
  }

  assert {
    condition = (
      length(aws_autoscaling_group.worker.initial_lifecycle_hook) == 2 &&
      one([
        for hook in aws_autoscaling_group.worker.initial_lifecycle_hook : hook
        if hook.lifecycle_transition == "autoscaling:EC2_INSTANCE_LAUNCHING"
      ]).heartbeat_timeout == 321 &&
      one([
        for hook in aws_autoscaling_group.worker.initial_lifecycle_hook : hook
        if hook.lifecycle_transition == "autoscaling:EC2_INSTANCE_LAUNCHING"
      ]).default_result == "ABANDON" &&
      one([
        for hook in aws_autoscaling_group.worker.initial_lifecycle_hook : hook
        if hook.lifecycle_transition == "autoscaling:EC2_INSTANCE_TERMINATING"
      ]).heartbeat_timeout == var.termination_lifecycle_heartbeat_timeout_seconds &&
      one([
        for hook in aws_autoscaling_group.worker.initial_lifecycle_hook : hook
        if hook.lifecycle_transition == "autoscaling:EC2_INSTANCE_TERMINATING"
      ]).default_result == "CONTINUE" &&
      output.termination_lifecycle_hook_name == "helmr-test-run-worker-terminate" &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "helmr-test-run-worker-launch") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "helmr-test-run-worker-terminate")
    )
    error_message = "every Worker ASG must install the launch-readiness and host-owned termination hooks"
  }

  assert {
    condition = (
      toset(aws_launch_template.worker.vpc_security_group_ids) == toset([aws_security_group.worker.id]) &&
      aws_launch_template.worker.iam_instance_profile[0].name == aws_iam_instance_profile.worker.name &&
      aws_launch_template.worker.image_id == var.ami_id
    )
    error_message = "the launch template must pin the Execution security group, instance profile, and AMI"
  }

  assert {
    condition = (
      aws_launch_template.worker.metadata_options[0].http_endpoint == "enabled" &&
      aws_launch_template.worker.metadata_options[0].http_tokens == "required" &&
      aws_launch_template.worker.metadata_options[0].http_put_response_hop_limit == 1
    )
    error_message = "worker metadata must require IMDSv2 with a one-hop response limit"
  }

  assert {
    condition = (
      length(aws_security_group.worker.ingress) == 0 &&
      aws_vpc_security_group_egress_rule.worker.security_group_id == aws_security_group.worker.id &&
      aws_vpc_security_group_egress_rule.worker.cidr_ipv4 == "0.0.0.0/0" &&
      aws_vpc_security_group_egress_rule.worker.ip_protocol == "-1"
    )
    error_message = "the worker security group must have no ingress and only explicit public-capable egress"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "ExecStart=helmr-worker") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_NETWORK_BLOCKED_IPV4_CIDRS=[\"10.0.0.0/8\",\"169.254.0.0/16\"]") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_NETWORK_LINK_POOL=169.254.64.0/18") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_NETWORK_RESOLVER_IPV4=10.20.0.2") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_NETWORK_TRANSLATION_POOL=100.96.0.0/16")
    )
    error_message = "worker user data must receive the complete generic routed-network configuration"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), var.secret_arns.worker_enrollment_token) &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_ENROLLMENT_TOKEN_FILE=%s") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "/run/helmr/worker-enrollment-token") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "helmr-worker-enrollment-token.service") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "Wants=helmr-worker-enrollment-token.service") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "After=helmr-worker-enrollment-token.service") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "Restart=on-failure") &&
      !strcontains(base64decode(aws_launch_template.worker.user_data), "Requires=helmr-worker-enrollment-token.service") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_RESOURCE_ID=%s") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "meta-data/instance-id")
    )
    error_message = "worker bootstrap must refresh the volatile enrollment token without making an existing credential depend on the secret store, and report only an opaque host locator"
  }

  assert {
    condition     = strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_DISK_RESERVE_MIB=1024")
    error_message = "worker user data must pin the disk reserve used by configured capacity math"
  }

  assert {
    condition = (
      aws_iam_role.worker.permissions_boundary == aws_iam_policy.worker_boundary.arn &&
      length(jsondecode(aws_iam_policy.worker_boundary.policy).Statement) == length(jsondecode(aws_iam_role_policy.worker.policy).Statement) + 1 &&
      alltrue([
        for statement in jsondecode(aws_iam_role_policy.worker.policy).Statement :
        contains([for boundary_statement in jsondecode(aws_iam_policy.worker_boundary.policy).Statement : jsonencode(boundary_statement)], jsonencode(statement))
      ]) &&
      toset(one([
        for statement in jsondecode(aws_iam_policy.worker_boundary.policy).Statement : statement.Action
        if try(statement.Sid, "") == "SSMManagedInstanceCore"
        ])) == toset([
        "ec2messages:AcknowledgeMessage",
        "ec2messages:DeleteMessage",
        "ec2messages:FailMessage",
        "ec2messages:GetEndpoint",
        "ec2messages:GetMessages",
        "ec2messages:SendReply",
        "ssm:DescribeAssociation",
        "ssm:DescribeDocument",
        "ssm:GetDeployablePatchSnapshotForInstance",
        "ssm:GetDocument",
        "ssm:GetManifest",
        "ssm:GetParameter",
        "ssm:GetParameters",
        "ssm:ListAssociations",
        "ssm:ListInstanceAssociations",
        "ssm:PutComplianceItems",
        "ssm:PutConfigurePackageResult",
        "ssm:PutInventory",
        "ssm:UpdateAssociationStatus",
        "ssm:UpdateInstanceAssociationStatus",
        "ssm:UpdateInstanceInformation",
        "ssmmessages:CreateControlChannel",
        "ssmmessages:CreateDataChannel",
        "ssmmessages:OpenControlChannel",
        "ssmmessages:OpenDataChannel"
      ]) &&
      !contains(flatten([for statement in jsondecode(aws_iam_policy.worker_boundary.policy).Statement : tolist(statement.Action)]), "*")
    )
    error_message = "every worker role must have its exact permissions plus only explicit SSM core actions bounded by a mandatory permissions boundary"
  }

  assert {
    condition = toset(one([
      for statement in jsondecode(aws_iam_role_policy.worker.policy).Statement : statement.Action
      if contains(statement.Action, "autoscaling:RecordLifecycleActionHeartbeat")
      ])) == toset([
      "autoscaling:CompleteLifecycleAction",
      "autoscaling:RecordLifecycleActionHeartbeat",
    ])
    error_message = "the Worker host must own lifecycle heartbeat and completion for its exact ASG"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "PLATFORM_STORE_URI=s3://helmr-test-runtime/objects") &&
      strcontains(aws_iam_role_policy.worker.policy, "${var.platform_store_bucket_arn}/objects/sha256/*") &&
      strcontains(aws_iam_role_policy.worker.policy, var.platform_store_kms_key_arn) &&
      !strcontains(aws_iam_role_policy.worker.policy, "CreatePlatformObjects") &&
      !strcontains(aws_iam_role_policy.worker.policy, "EncryptPlatformObjects") &&
      !strcontains(aws_iam_role_policy.worker.policy, "AssumeExecutionImageCacheRole") &&
      !strcontains(base64decode(aws_launch_template.worker.user_data), "IMAGE_CACHE_")
    )
    error_message = "run-only workers must receive read-only Platform Artifact authority without image-cache config or IAM"
  }

  assert {
    condition     = !strcontains(base64decode(aws_launch_template.worker.user_data), "BUILD_POLICY_PATH") && !strcontains(base64decode(aws_launch_template.worker.user_data), "release install")
    error_message = "run-only workers must not receive or install the current build policy"
  }

  assert {
    condition = (
      !strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_BUILD_CACHE_DIR") &&
      !strcontains(base64decode(aws_launch_template.worker.user_data), "build-cache.ext4") &&
      !strcontains(base64decode(aws_launch_template.worker.user_data), "helmr-buildkit")
    )
    error_message = "run-only workers must not receive build filesystems or a host BuildKit service"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "launch_timeout='321'") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "systemctl is-active --quiet 'helmr-worker' || return 1") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "\"$worker_binary\" status >/dev/null || return 1") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "drain-complete") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "ABANDON")
    )
    error_message = "worker lifecycle handling must bound launch readiness and bypass repeated drain after durable local completion"
  }
}

run "worker_without_ssm_has_exact_permission_boundary" {
  command = plan

  variables {
    enable_ssm = false
  }

  assert {
    condition = (
      aws_iam_role.worker.permissions_boundary == aws_iam_policy.worker_boundary.arn &&
      jsondecode(aws_iam_policy.worker_boundary.policy) == jsondecode(aws_iam_role_policy.worker.policy)
    )
    error_message = "workers without SSM must retain a mandatory boundary exactly equal to their worker permissions"
  }
}

run "build_worker_installs_exact_policy_before_service" {
  command = plan

  variables {
    name                       = "helmr-test-build"
    worker_roles               = ["build"]
    worker_capacity_vcpus      = 4
    worker_capacity_memory_mib = 8192
    worker_execution_slots     = 1
    vm_vcpus                   = 3
    vm_memory_mib              = 4096
    vm_scratch_disk_mib        = 32768
    build_policy_digest        = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    build_cache_mib            = 8192
    build_scratch_mib          = 34816
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "BUILD_POLICY_PATH=/etc/helmr/build-policy.json") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "\"helmr-worker\" release install") &&
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
      !strcontains(aws_iam_role_policy.worker.policy, "${var.platform_store_bucket_arn}/controlplane/runtime")
    )
    error_message = "build worker IAM must publish immutable Platform candidates without rollout lineage authority"
  }

  assert {
    condition = (
      strcontains(base64decode(aws_launch_template.worker.user_data), "build-cache.ext4") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "build-scratch.ext4") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "cache_headroom_bytes=$((512 * 1048576 + cache_usable_bytes / 100))") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "scratch_headroom_bytes=$((512 * 1048576 + scratch_usable_bytes / 100))") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "cache_raw_bytes=$((cache_usable_bytes + cache_headroom_bytes))") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "scratch_raw_bytes=$((scratch_usable_bytes + scratch_headroom_bytes))") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "mkfs.ext4 -F -q -m 0") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "[ \"$allocated_bytes\" -ge \"$raw_bytes\" ]") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "did not preserve its fixed reserve after build filesystem allocation") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "Options=loop,nosuid,nodev,nodiscard") &&
      !strcontains(base64decode(aws_launch_template.worker.user_data), "helmr-buildkit") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "WORKER_WORK_DIR=/var/lib/helmr/scratch/worker") &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "JAILER_CHROOT_DIR=/var/lib/helmr/scratch/jailer")
    )
    error_message = "build workers must mount distinct fixed ext4 filesystems for Worker cache and image-build VM scratch without host BuildKit"
  }

  assert {
    condition = (
      one([for statement in jsondecode(aws_iam_role_policy.worker.policy).Statement : statement if try(statement.Sid, "") == "AssumeExecutionImageCacheRole"]) == {
        Action   = ["sts:AssumeRole"]
        Effect   = "Allow"
        Resource = var.image_cache_role_arn
        Sid      = "AssumeExecutionImageCacheRole"
      } &&
      one([for statement in jsondecode(aws_iam_policy.worker_boundary.policy).Statement : statement if try(statement.Sid, "") == "AssumeExecutionImageCacheRole"]) == {
        Action   = ["sts:AssumeRole"]
        Effect   = "Allow"
        Resource = var.image_cache_role_arn
        Sid      = "AssumeExecutionImageCacheRole"
      } &&
      strcontains(base64decode(aws_launch_template.worker.user_data), "IMAGE_CACHE_REPOSITORY_ARN_PREFIX=${var.image_cache_repository_arn_prefix}")
    )
    error_message = "build Workers must receive the exact ECR cache config and independently allow only the exact cache role in their identity policy and mandatory boundary"
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

run "explicit_disk_must_exceed_reserve" {
  command = plan

  variables {
    worker_disk_mib         = 1024
    worker_disk_reserve_mib = 1024
  }

  expect_failures = [terraform_data.network_preconditions]
}
