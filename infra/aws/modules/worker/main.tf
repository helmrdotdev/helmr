locals {
  name                  = lower(var.name)
  asg_name              = "${local.name}-worker"
  launch_hook_name      = "${local.name}-worker-launch"
  termination_hook_name = "${local.name}-worker-terminate"
  network_resolver_ipv4 = coalesce(var.network_resolver_ipv4, cidrhost(data.aws_vpc.selected.cidr_block, 2))

  worker_environment_values = {
    CONTROL_PLANE_URL                 = var.worker_controlplane_url
    CAS_URI                           = var.cas_uri
    PLATFORM_STORE_URI                = var.platform_store_uri
    FIRECRACKER_PATH                  = "/usr/local/bin/firecracker"
    CPU_TEMPLATE_HELPER_PATH          = "/usr/local/bin/cpu-template-helper"
    JAILER_PATH                       = "/usr/local/bin/jailer"
    JAILER_UID                        = tostring(var.jailer_uid)
    JAILER_GID                        = tostring(var.jailer_gid)
    JAILER_CGROUP_VERSION             = "2"
    WORKER_NETWORK_BLOCKED_IPV4_CIDRS = jsonencode(var.network_blocked_ipv4_cidrs)
    WORKER_NETWORK_LINK_POOL          = var.network_link_pool
    WORKER_NETWORK_RESOLVER_IPV4      = local.network_resolver_ipv4
    WORKER_NETWORK_TRANSLATION_POOL   = var.network_translation_pool
    WORKER_WORK_DIR                   = "/var/lib/helmr"
    WORKER_INSTANCE_CREDENTIAL_PATH   = "/var/lib/helmr/worker-credential.json"
    WORKER_POOL_NAME                  = var.worker_pool_name
    WORKER_IMAGES_DIR                 = "/var/lib/helmr/images"
    JAILER_CHROOT_DIR                 = "/var/lib/helmr/jailer"
    VM_VCPUS                          = tostring(var.vm_vcpus)
    VM_MEMORY_MIB                     = tostring(var.vm_memory_mib)
    VM_SCRATCH_DISK_MIB               = tostring(var.vm_scratch_disk_mib)
    VM_INIT_TIMEOUT                   = "30s"
    # EC2 workers allow extra time for first-boot guest health convergence.
    VM_HEALTH_TIMEOUT              = "300s"
    WORKER_DISK_RESERVE_MIB        = tostring(var.worker_disk_reserve_mib)
    WORKER_DISK_MIB                = var.worker_disk_mib == null ? null : tostring(var.worker_disk_mib)
    WORKER_CAPACITY_VCPUS          = var.worker_capacity_vcpus == null ? null : tostring(var.worker_capacity_vcpus)
    WORKER_CAPACITY_MEMORY_MIB     = var.worker_capacity_memory_mib == null ? null : tostring(var.worker_capacity_memory_mib)
    WORKER_EXECUTION_SLOTS         = var.worker_execution_slots == null ? null : tostring(var.worker_execution_slots)
    WORKER_SUBSTRATE_CACHE_MAX_MIB = var.substrate_cache_max_mib == null ? null : tostring(var.substrate_cache_max_mib)
    WORKER_ARTIFACT_CACHE_MAX_MIB  = var.artifact_cache_max_mib == null ? null : tostring(var.artifact_cache_max_mib)
  }
  worker_environment = {
    for key, value in local.worker_environment_values : key => value if value != null
  }

  reserved_worker_environment_keys = setunion(toset(keys(local.worker_environment_values)), toset([
    "AWS_REGION",
    "AWS_DEFAULT_REGION",
    "CHECKPOINT_ENCRYPTION_KEY",
    "WORKER_ENROLLMENT_TOKEN_FILE",
    "WORKER_RESOURCE_ID",
  ]))
  worker_environment_conflicts = setintersection(keys(var.worker_environment), local.reserved_worker_environment_keys)
  base_worker_environment      = merge(var.worker_environment, local.worker_environment)
  worker_user_data_max_bytes   = 15360
  worker_user_data = templatefile("${path.module}/templates/user-data.sh.tftpl", {
    environment                          = local.base_worker_environment
    checkpoint_key_secret_arn            = var.secret_arns.checkpoint_encryption_key
    worker_enrollment_token_secret_arn   = var.secret_arns.worker_enrollment_token
    worker_service_name                  = var.worker_service_name
    worker_binary_path                   = var.worker_binary_path
    autoscaling_group_name               = local.asg_name
    launch_lifecycle_hook_name           = local.launch_hook_name
    launch_readiness_timeout_seconds     = var.launch_lifecycle_heartbeat_timeout_seconds
    termination_lifecycle_hook_name      = local.termination_hook_name
    termination_drain_timeout_seconds    = var.termination_drain_timeout_seconds
    lifecycle_heartbeat_interval_seconds = var.lifecycle_heartbeat_interval_seconds
    worker_work_dir                      = local.base_worker_environment.WORKER_WORK_DIR
    aws_region                           = data.aws_region.current.region
    expected_root_bytes                  = format("%.0f", var.root_volume_size_gb * 1073741824)
  })
  rendered_worker_user_data_base64 = base64encode(local.worker_user_data)
  worker_user_data_base64 = var.sealed_provider_definition == null ? (
    local.rendered_worker_user_data_base64
  ) : var.sealed_provider_definition.user_data_base64
  worker_user_data_size_bytes = (
    length(local.worker_user_data_base64) * 3 / 4 -
    length(regexall("=", local.worker_user_data_base64))
  )

  worker_permission_policy = {
    Version = "2012-10-17"
    Statement = concat([
      {
        Effect = "Allow"
        Action = [
          "s3:GetObject",
          "s3:PutObject",
          "s3:PutObjectTagging",
          "s3:AbortMultipartUpload",
          "s3:ListBucket"
        ]
        Resource = [
          var.cas_bucket_arn,
          "${var.cas_bucket_arn}/*"
        ]
      },
      {
        Effect = "Allow"
        Action = [
          "autoscaling:CompleteLifecycleAction",
          "autoscaling:RecordLifecycleActionHeartbeat"
        ]
        Resource = "arn:aws:autoscaling:*:*:autoScalingGroup:*:autoScalingGroupName/${local.asg_name}"
      },
      {
        Effect = "Allow"
        Action = [
          "kms:Decrypt",
          "kms:Encrypt",
          "kms:GenerateDataKey"
        ]
        Resource = var.kms_key_arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = [
              "s3.${data.aws_region.current.region}.amazonaws.com",
              "secretsmanager.${data.aws_region.current.region}.amazonaws.com"
            ]
          }
        }
      },
      {
        Effect = "Allow"
        Action = [
          "secretsmanager:GetSecretValue"
        ]
        Resource = [
          var.secret_arns.checkpoint_encryption_key,
          var.secret_arns.worker_enrollment_token,
        ]
      },
      ], [
      {
        Sid    = "ReadPlatformObjects"
        Effect = "Allow"
        Action = [
          "s3:GetObject"
        ]
        Resource = "${var.platform_store_bucket_arn}/objects/sha256/*"
      },
      {
        Sid    = "DecryptPlatformObjects"
        Effect = "Allow"
        Action = [
          "kms:Decrypt"
        ]
        Resource = var.platform_store_kms_key_arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = "s3.${data.aws_region.current.region}.amazonaws.com"
          }
        }
      },
    ])
  }
  worker_boundary_policy = {
    Version = local.worker_permission_policy.Version
    Statement = concat(local.worker_permission_policy.Statement, var.enable_ssm ? [{
      Sid    = "SSMManagedInstanceCore"
      Effect = "Allow"
      Action = [
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
      ]
      Resource = "*"
    }] : [])
  }
  worker_permission_policy_json = var.sealed_provider_definition == null ? (
    jsonencode(local.worker_permission_policy)
  ) : var.sealed_provider_definition.permission_policy_json
  worker_boundary_policy_json = var.sealed_provider_definition == null ? (
    jsonencode(local.worker_boundary_policy)
  ) : var.sealed_provider_definition.boundary_policy_json
  worker_enable_ssm = var.sealed_provider_definition == null ? (
    var.enable_ssm
  ) : var.sealed_provider_definition.enable_ssm
  worker_health_check_grace_period_seconds               = var.sealed_provider_definition == null ? var.health_check_grace_period_seconds : var.sealed_provider_definition.health_check_grace_period_seconds
  worker_launch_lifecycle_heartbeat_timeout_seconds      = var.sealed_provider_definition == null ? var.launch_lifecycle_heartbeat_timeout_seconds : var.sealed_provider_definition.launch_lifecycle_heartbeat_timeout_seconds
  worker_termination_lifecycle_heartbeat_timeout_seconds = var.sealed_provider_definition == null ? var.termination_lifecycle_heartbeat_timeout_seconds : var.sealed_provider_definition.termination_lifecycle_heartbeat_timeout_seconds
  worker_termination_drain_timeout_seconds               = var.sealed_provider_definition == null ? var.termination_drain_timeout_seconds : var.sealed_provider_definition.termination_drain_timeout_seconds
  worker_lifecycle_heartbeat_interval_seconds            = var.sealed_provider_definition == null ? var.lifecycle_heartbeat_interval_seconds : var.sealed_provider_definition.lifecycle_heartbeat_interval_seconds
  worker_termination_policies                            = var.sealed_provider_definition == null ? ["OldestLaunchTemplate", "OldestInstance"] : var.sealed_provider_definition.termination_policies
  worker_protect_from_scale_in                           = var.sealed_provider_definition == null ? true : var.sealed_provider_definition.protect_from_scale_in
  worker_health_check_type                               = var.sealed_provider_definition == null ? "EC2" : var.sealed_provider_definition.health_check_type
  worker_instance_refresh_strategy                       = var.sealed_provider_definition == null ? "Rolling" : var.sealed_provider_definition.instance_refresh_strategy
  worker_instance_refresh_min_healthy_percentage         = var.sealed_provider_definition == null ? 100 : var.sealed_provider_definition.instance_refresh_min_healthy_percentage
  worker_instance_refresh_max_healthy_percentage         = var.sealed_provider_definition == null ? 100 : var.sealed_provider_definition.instance_refresh_max_healthy_percentage
  worker_instance_refresh_scale_in_protected_instances   = var.sealed_provider_definition == null ? "Refresh" : var.sealed_provider_definition.instance_refresh_scale_in_protected_instances
  worker_instance_refresh_standby_instances              = var.sealed_provider_definition == null ? "Terminate" : var.sealed_provider_definition.instance_refresh_standby_instances
  worker_instance_refresh_skip_matching                  = var.sealed_provider_definition == null ? true : var.sealed_provider_definition.instance_refresh_skip_matching
  worker_launch_lifecycle_transition                     = var.sealed_provider_definition == null ? "autoscaling:EC2_INSTANCE_LAUNCHING" : var.sealed_provider_definition.launch_lifecycle_transition
  worker_launch_lifecycle_default_result                 = var.sealed_provider_definition == null ? "ABANDON" : var.sealed_provider_definition.launch_lifecycle_default_result
  worker_termination_lifecycle_transition                = var.sealed_provider_definition == null ? "autoscaling:EC2_INSTANCE_TERMINATING" : var.sealed_provider_definition.termination_lifecycle_transition
  worker_termination_lifecycle_default_result            = var.sealed_provider_definition == null ? "CONTINUE" : var.sealed_provider_definition.termination_lifecycle_default_result
}

resource "aws_security_group" "worker" {
  name        = "${local.name}-worker"
  description = "Helmr worker instances"
  vpc_id      = var.vpc_id
  tags        = var.tags
}

resource "aws_vpc_security_group_egress_rule" "worker" {
  security_group_id = aws_security_group.worker.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_iam_policy" "worker_boundary" {
  name   = "${local.name}-worker-boundary"
  policy = local.worker_boundary_policy_json
  tags   = var.tags
}

resource "aws_iam_role" "worker" {
  name                 = "${local.name}-worker"
  permissions_boundary = aws_iam_policy.worker_boundary.arn
  tags                 = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "worker" {
  name = "${local.name}-worker"
  role = aws_iam_role.worker.id

  policy = local.worker_permission_policy_json
}

resource "aws_iam_role_policy_attachment" "ssm" {
  count = local.worker_enable_ssm ? 1 : 0

  role       = aws_iam_role.worker.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_instance_profile" "worker" {
  name = "${local.name}-worker"
  role = aws_iam_role.worker.name
  tags = var.tags
}

resource "aws_launch_template" "worker" {
  name_prefix            = "${local.name}-worker-"
  image_id               = var.ami_id
  instance_type          = var.instance_type
  vpc_security_group_ids = [aws_security_group.worker.id]
  user_data              = local.worker_user_data_base64

  lifecycle {
    precondition {
      condition     = local.worker_user_data_size_bytes <= local.worker_user_data_max_bytes
      error_message = "Worker user data must stay within 15360 decoded bytes to preserve headroom below the EC2 16384-byte limit."
    }
  }

  iam_instance_profile {
    name = aws_iam_instance_profile.worker.name
  }

  dynamic "cpu_options" {
    for_each = var.enable_nested_virtualization ? [1] : []

    content {
      nested_virtualization = "enabled"
    }
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  block_device_mappings {
    device_name = var.root_volume_device_name

    ebs {
      volume_size           = var.root_volume_size_gb
      volume_type           = var.root_volume_type
      iops                  = var.root_volume_iops
      throughput            = var.root_volume_throughput
      encrypted             = true
      delete_on_termination = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags          = merge(var.tags, { Name = "${local.name}-worker" })
  }

  tags = var.tags
}

resource "terraform_data" "network_preconditions" {
  input = {
    network_policy = jsonencode({
      blocked_ipv4_cidrs = var.network_blocked_ipv4_cidrs
      link_pool          = var.network_link_pool
      resolver_ipv4      = local.network_resolver_ipv4
      translation_pool   = var.network_translation_pool
    })
    reserved_env_conflicts = local.worker_environment_conflicts
    platform_store_uri     = var.platform_store_uri
  }

  lifecycle {
    precondition {
      condition     = length(local.worker_environment_conflicts) == 0
      error_message = "worker_environment must not set module-managed Worker variables. Remove conflicting entries and use typed inputs where available."
    }

    precondition {
      condition     = var.platform_store_uri == "s3://${trimprefix(var.platform_store_bucket_arn, "arn:${data.aws_partition.current.partition}:s3:::")}/objects"
      error_message = "platform_store_uri must identify the bucket supplied by platform_store_bucket_arn and end in /objects."
    }

    precondition {
      condition     = var.platform_store_bucket_arn != var.cas_bucket_arn
      error_message = "platform_store_bucket_arn must identify the dedicated bootstrap store, not the mutable Artifact CAS bucket."
    }

    precondition {
      condition     = var.worker_disk_mib == null || var.worker_disk_mib > var.worker_disk_reserve_mib
      error_message = "worker_disk_mib must exceed worker_disk_reserve_mib when an explicit filesystem capacity is configured."
    }
  }
}

resource "aws_autoscaling_group" "worker" {
  name                      = local.asg_name
  min_size                  = var.min_size
  max_size                  = var.max_size
  desired_capacity          = null
  protect_from_scale_in     = local.worker_protect_from_scale_in
  vpc_zone_identifier       = var.subnet_ids
  health_check_type         = local.worker_health_check_type
  health_check_grace_period = local.worker_health_check_grace_period_seconds
  termination_policies      = local.worker_termination_policies

  launch_template {
    id      = aws_launch_template.worker.id
    version = var.sealed_provider_definition == null ? aws_launch_template.worker.latest_version : var.sealed_provider_definition.launch_template_version
  }

  instance_refresh {
    strategy = local.worker_instance_refresh_strategy

    preferences {
      min_healthy_percentage       = local.worker_instance_refresh_min_healthy_percentage
      max_healthy_percentage       = local.worker_instance_refresh_max_healthy_percentage
      scale_in_protected_instances = local.worker_instance_refresh_scale_in_protected_instances
      standby_instances            = local.worker_instance_refresh_standby_instances
      skip_matching                = local.worker_instance_refresh_skip_matching
    }
  }

  initial_lifecycle_hook {
    name                 = local.launch_hook_name
    lifecycle_transition = local.worker_launch_lifecycle_transition
    heartbeat_timeout    = local.worker_launch_lifecycle_heartbeat_timeout_seconds
    default_result       = local.worker_launch_lifecycle_default_result
  }

  initial_lifecycle_hook {
    name                 = local.termination_hook_name
    lifecycle_transition = local.worker_termination_lifecycle_transition
    heartbeat_timeout    = local.worker_termination_lifecycle_heartbeat_timeout_seconds
    default_result       = local.worker_termination_lifecycle_default_result
  }

  tag {
    key                 = "Name"
    value               = "${local.name}-worker"
    propagate_at_launch = true
  }

  lifecycle {
    precondition {
      condition     = var.min_size <= var.max_size
      error_message = "worker capacity must satisfy min_size <= max_size."
    }
  }
}
