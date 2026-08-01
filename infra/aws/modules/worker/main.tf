locals {
  name                    = lower(var.name)
  asg_name                = "${local.name}-worker"
  launch_hook_name        = "${local.name}-worker-launch"
  termination_hook_name   = "${local.name}-worker-terminate"
  boot_corpus_reserve_mib = 2048
  build_scratch_min_mib   = max(32768, var.vm_scratch_disk_mib) + local.boot_corpus_reserve_mib
  network_resolver_ipv4   = coalesce(var.network_resolver_ipv4, cidrhost(data.aws_vpc.selected.cidr_block, 2))

  disk_environment = merge({
    HELMR_WORKER_DISK_RESERVE_MIB = tostring(var.worker_disk_reserve_mib)
    }, var.worker_disk_mib == null ? {} : {
    HELMR_WORKER_DISK_MIB = tostring(var.worker_disk_mib)
  })
  capacity_environment = merge(
    var.worker_capacity_vcpus == null ? {} : {
      HELMR_WORKER_CAPACITY_VCPUS = tostring(var.worker_capacity_vcpus)
    },
    var.worker_capacity_memory_mib == null ? {} : {
      HELMR_WORKER_CAPACITY_MEMORY_MIB = tostring(var.worker_capacity_memory_mib)
    },
    var.worker_execution_slots == null ? {} : {
      HELMR_WORKER_EXECUTION_SLOTS = tostring(var.worker_execution_slots)
    },
  )
  cache_environment = merge(
    var.substrate_cache_max_mib == null ? {} : {
      HELMR_WORKER_SUBSTRATE_CACHE_MAX_MIB = tostring(var.substrate_cache_max_mib)
    },
    var.artifact_cache_max_mib == null ? {} : {
      HELMR_WORKER_ARTIFACT_CACHE_MAX_MIB = tostring(var.artifact_cache_max_mib)
    },
  )

  worker_environment = merge({
    HELMR_CONTROL_URL                       = var.worker_control_url
    HELMR_CAS_URI                           = var.cas_uri
    HELMR_PLATFORM_STORE_URI                = var.platform_store_uri
    HELMR_WORKER_GROUP_ID                   = var.worker_group_id
    HELMR_WORKER_FIRECRACKER_PATH           = "/usr/local/bin/firecracker"
    HELMR_WORKER_FIRECRACKER_JAILER_PATH    = "/usr/local/bin/jailer"
    HELMR_WORKER_FIRECRACKER_JAILER_UID     = tostring(var.jailer_uid)
    HELMR_WORKER_FIRECRACKER_JAILER_GID     = tostring(var.jailer_gid)
    HELMR_WORKER_FIRECRACKER_CGROUP_VERSION = "2"
    HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS = jsonencode(var.network_blocked_ipv4_cidrs)
    HELMR_WORKER_NETWORK_LINK_POOL          = var.network_link_pool
    HELMR_WORKER_NETWORK_RESOLVER_IPV4      = local.network_resolver_ipv4
    HELMR_WORKER_NETWORK_TRANSLATION_POOL   = var.network_translation_pool
    HELMR_WORKER_WORK_DIR                   = contains(var.worker_roles, "build") ? "/var/lib/helmr/scratch/worker" : "/var/lib/helmr"
    HELMR_WORKER_INSTANCE_CREDENTIAL_PATH   = "/var/lib/helmr/worker-credential.json"
    HELMR_WORKER_ROLES                      = join(",", sort(tolist(var.worker_roles)))
    HELMR_WORKER_IMAGES_DIR                 = "/var/lib/helmr/images"
    HELMR_WORKER_FIRECRACKER_CHROOT_DIR     = contains(var.worker_roles, "build") ? "/var/lib/helmr/scratch/jailer" : "/var/lib/helmr/jailer"
    HELMR_VM_VCPUS                          = tostring(var.vm_vcpus)
    HELMR_VM_MEMORY_MIB                     = tostring(var.vm_memory_mib)
    HELMR_VM_SCRATCH_DISK_MIB               = tostring(var.vm_scratch_disk_mib)
    HELMR_VM_INIT_TIMEOUT                   = "30s"
    HELMR_VM_HEALTH_TIMEOUT                 = "300s"
    }, contains(var.worker_roles, "build") ? {
    HELMR_BUILD_POLICY_PATH                 = "/etc/helmr/build-policy.json"
    HELMR_WORKER_BUILD_CACHE_DIR            = "/var/lib/helmr/cache"
    HELMR_WORKER_BUILD_SCRATCH_DIR          = "/var/lib/helmr/scratch"
    HELMR_IMAGE_CACHE_REGISTRY_AUTHORITY    = var.image_cache_registry_authority
    HELMR_IMAGE_CACHE_REPOSITORY_PREFIX     = var.image_cache_repository_prefix
    HELMR_IMAGE_CACHE_ROLE_ARN              = var.image_cache_role_arn
    HELMR_IMAGE_CACHE_REPOSITORY_ARN_PREFIX = var.image_cache_repository_arn_prefix
  } : {}, local.disk_environment, local.capacity_environment, local.cache_environment)

  reserved_worker_environment_keys = toset(concat(keys(local.worker_environment), [
    "CHECKPOINT_ENCRYPTION_KEY",
    "HELMR_BUILD_POLICY_PATH",
    "HELMR_PLATFORM_STORE_URI",
  ]))
  worker_environment_conflicts = setintersection(keys(var.worker_environment), local.reserved_worker_environment_keys)
  base_worker_environment      = merge(local.worker_environment, var.worker_environment)

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
          var.secret_arns.worker_enrollment,
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
      ], [
      for statement in [
        {
          Sid      = "AssumeExecutionImageCacheRole"
          Effect   = "Allow"
          Action   = ["sts:AssumeRole"]
          Resource = var.image_cache_role_arn
        },
        {
          Sid    = "CreatePlatformObjects"
          Effect = "Allow"
          Action = [
            "s3:PutObject",
            "s3:AbortMultipartUpload",
            "s3:ListMultipartUploadParts"
          ]
          Resource = "${var.platform_store_bucket_arn}/objects/sha256/*"
        },
        {
          Sid    = "EncryptPlatformObjects"
          Effect = "Allow"
          Action = [
            "kms:Encrypt",
            "kms:GenerateDataKey"
          ]
          Resource = var.platform_store_kms_key_arn
          Condition = {
            StringEquals = {
              "kms:ViaService" = "s3.${data.aws_region.current.region}.amazonaws.com"
            }
          }
        },
      ] : statement if contains(var.worker_roles, "build")
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
  policy = jsonencode(local.worker_boundary_policy)
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

  policy = jsonencode(local.worker_permission_policy)
}

resource "aws_iam_role_policy_attachment" "ssm" {
  count = var.enable_ssm ? 1 : 0

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
  user_data = base64encode(templatefile("${path.module}/templates/user-data.sh.tftpl", {
    environment                          = local.base_worker_environment
    checkpoint_key_secret_arn            = var.secret_arns.checkpoint_encryption_key
    worker_enrollment_secret_arn         = var.secret_arns.worker_enrollment
    worker_supports_build                = contains(var.worker_roles, "build")
    worker_service_name                  = var.worker_service_name
    worker_binary_path                   = var.worker_binary_path
    autoscaling_group_name               = local.asg_name
    launch_lifecycle_hook_name           = var.enable_lifecycle_hooks ? local.launch_hook_name : ""
    launch_readiness_timeout_seconds     = var.launch_lifecycle_heartbeat_timeout_seconds
    termination_lifecycle_hook_name      = var.enable_lifecycle_hooks ? local.termination_hook_name : ""
    termination_drain_timeout_seconds    = var.termination_drain_timeout_seconds
    lifecycle_heartbeat_interval_seconds = var.lifecycle_heartbeat_interval_seconds
    worker_work_dir                      = local.base_worker_environment.HELMR_WORKER_WORK_DIR
    aws_region                           = data.aws_region.current.region
    platform_store_uri                   = var.platform_store_uri
    build_policy_digest                  = var.build_policy_digest == null ? "" : var.build_policy_digest
    build_cache_mib                      = var.build_cache_mib == null ? 0 : var.build_cache_mib
    build_scratch_mib                    = var.build_scratch_mib == null ? 0 : var.build_scratch_mib
    worker_disk_reserve_mib              = var.worker_disk_reserve_mib
  }))

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
    build_policy_digest    = var.build_policy_digest
    build_cache_mib        = var.build_cache_mib
    build_scratch_mib      = var.build_scratch_mib
  }

  lifecycle {
    precondition {
      condition     = length(local.worker_environment_conflicts) == 0
      error_message = "worker_environment must not set infra-owned HELMR_* routing or security variables. Use explicit worker module inputs instead."
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
      condition     = contains(var.worker_roles, "build") == (var.build_policy_digest != null)
      error_message = "build-capable workers require build_policy_digest; run-only workers must not receive the current build policy."
    }

    precondition {
      condition = contains(var.worker_roles, "build") == (
        var.build_cache_mib != null &&
        var.build_scratch_mib != null
      )
      error_message = "build-capable workers require build_cache_mib and build_scratch_mib; run-only workers must not allocate build filesystems."
    }

    precondition {
      condition = !contains(var.worker_roles, "build") || (
        var.worker_disk_reserve_mib >= 1024 &&
        coalesce(var.build_scratch_mib, 0) >= local.build_scratch_min_mib
      )
      error_message = "build workers require at least 1024 MiB of unadvertised root reserve and a two-GiB boot-corpus reserve beyond the larger of the fixed build envelope and configured VM scratch."
    }

    precondition {
      condition     = var.worker_disk_mib == null || var.worker_disk_mib > var.worker_disk_reserve_mib
      error_message = "worker_disk_mib must exceed worker_disk_reserve_mib when an explicit filesystem capacity is configured."
    }

    precondition {
      condition = !contains(var.worker_roles, "build") || (
        var.vm_vcpus >= 3 &&
        var.vm_memory_mib >= 4096 &&
        var.vm_scratch_disk_mib >= 32768
      )
      error_message = "build workers require a VM shape that fits the fixed 3000 milli-CPU, 4096 MiB, and 32768 MiB image-build guest."
    }

    precondition {
      condition = !contains(var.worker_roles, "build") || (
        var.worker_capacity_vcpus != null &&
        var.worker_capacity_memory_mib != null &&
        var.worker_capacity_vcpus >= (contains(var.worker_roles, "run") ? max(3, var.vm_vcpus + 1) : 3) &&
        var.worker_capacity_memory_mib >= (contains(var.worker_roles, "run") ? max(4096, var.vm_memory_mib + 2048) : 4096)
      )
      error_message = "build workers require a host pool that fits the fixed image-build guest and any configured run VM shape."
    }
  }
}

resource "aws_autoscaling_group" "worker" {
  name                      = local.asg_name
  min_size                  = var.min_size
  max_size                  = var.max_size
  desired_capacity          = null
  protect_from_scale_in     = true
  vpc_zone_identifier       = var.subnet_ids
  health_check_type         = "EC2"
  health_check_grace_period = var.health_check_grace_period_seconds
  termination_policies      = ["OldestLaunchTemplate", "OldestInstance"]

  launch_template {
    id      = aws_launch_template.worker.id
    version = aws_launch_template.worker.latest_version
  }

  instance_refresh {
    strategy = "Rolling"

    preferences {
      min_healthy_percentage       = 100
      max_healthy_percentage       = 100
      scale_in_protected_instances = "Refresh"
      standby_instances            = "Terminate"
      skip_matching                = true
    }
  }

  dynamic "initial_lifecycle_hook" {
    for_each = var.enable_lifecycle_hooks ? [1] : []

    content {
      name                 = local.launch_hook_name
      lifecycle_transition = "autoscaling:EC2_INSTANCE_LAUNCHING"
      heartbeat_timeout    = var.launch_lifecycle_heartbeat_timeout_seconds
      default_result       = "ABANDON"
    }
  }

  dynamic "initial_lifecycle_hook" {
    for_each = var.enable_lifecycle_hooks ? [1] : []

    content {
      name                 = local.termination_hook_name
      lifecycle_transition = "autoscaling:EC2_INSTANCE_TERMINATING"
      heartbeat_timeout    = var.termination_lifecycle_heartbeat_timeout_seconds
      default_result       = "CONTINUE"
    }
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

    precondition {
      condition     = var.enable_lifecycle_hooks
      error_message = "worker groups require launch and termination lifecycle hooks."
    }
  }
}
