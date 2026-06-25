locals {
  name                  = lower(var.name)
  asg_name              = "${local.name}-worker"
  launch_hook_name      = "${local.name}-worker-launch"
  termination_hook_name = "${local.name}-worker-terminate"

  disk_environment = var.worker_disk_mib == null ? {} : {
    HELMR_WORKER_DISK_MIB = tostring(var.worker_disk_mib)
  }
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

  managed_worker_environment = merge({
    HELMR_CONTROL_URL                       = var.worker_control_url
    HELMR_CAS_URI                           = var.cas_uri
    HELMR_WORKER_REGION                     = data.aws_region.current.region
    HELMR_WORKER_BUILDKIT_ADDR              = "unix:///run/helmr/buildkit/buildkitd.sock"
    HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE   = local.name
    HELMR_WORKER_FIRECRACKER_PATH           = "/usr/local/bin/firecracker"
    HELMR_WORKER_FIRECRACKER_JAILER_PATH    = "/usr/local/bin/jailer"
    HELMR_WORKER_FIRECRACKER_JAILER_UID     = tostring(var.jailer_uid)
    HELMR_WORKER_FIRECRACKER_JAILER_GID     = tostring(var.jailer_gid)
    HELMR_WORKER_FIRECRACKER_CGROUP_VERSION = "2"
    HELMR_WORKER_CNI_NETWORK                = "helmr"
    HELMR_WORKER_CNI_PROFILE                = "helmr/v0"
    HELMR_WORKER_WORK_DIR                   = "/var/lib/helmr"
    HELMR_WORKER_INSTANCE_CREDENTIAL_PATH   = "/var/lib/helmr/worker-credential.json"
    HELMR_WORKER_BOOTSTRAP_TOKEN_PATH       = "/etc/helmr/worker-bootstrap-token"
    HELMR_WORKER_IMAGES_DIR                 = "/var/lib/helmr/images"
    HELMR_WORKER_FIRECRACKER_CHROOT_DIR     = "/var/lib/helmr/jailer"
    HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS = length(var.network_blocked_ipv4_cidrs) == 0 ? "none" : join(",", var.network_blocked_ipv4_cidrs)
    HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS = length(var.network_blocked_ipv6_cidrs) == 0 ? "none" : join(",", var.network_blocked_ipv6_cidrs)
    HELMR_VM_VCPUS                          = tostring(var.vm_vcpus)
    HELMR_VM_MEMORY_MIB                     = tostring(var.vm_memory_mib)
    HELMR_VM_SCRATCH_DISK_MIB               = tostring(var.vm_scratch_disk_mib)
    HELMR_VM_HEALTH_TIMEOUT                 = "300s"
  }, local.disk_environment, local.capacity_environment)

  reserved_worker_environment_keys = toset(concat(keys(local.managed_worker_environment), ["HELMR_CHECKPOINT_ENCRYPTION_KEY", "HELMR_WORKER_RESOURCE_ID"]))
  worker_environment_conflicts     = setintersection(keys(var.worker_environment), local.reserved_worker_environment_keys)
  base_worker_environment          = merge(local.managed_worker_environment, var.worker_environment)

  buildkit_slirp_cidr_parts   = regex("^([0-9]+)\\.([0-9]+)\\.([0-9]+)\\.([0-9]+)/([0-9]+)$", var.buildkit_slirp_cidr)
  buildkit_slirp_cidr_prefix  = tonumber(local.buildkit_slirp_cidr_parts[4])
  buildkit_slirp_cidr_address = tonumber(local.buildkit_slirp_cidr_parts[0]) * 16777216 + tonumber(local.buildkit_slirp_cidr_parts[1]) * 65536 + tonumber(local.buildkit_slirp_cidr_parts[2]) * 256 + tonumber(local.buildkit_slirp_cidr_parts[3])
  buildkit_slirp_cidr_size    = pow(2, 32 - local.buildkit_slirp_cidr_prefix)
  buildkit_slirp_cidr_start   = local.buildkit_slirp_cidr_address - local.buildkit_slirp_cidr_address % local.buildkit_slirp_cidr_size
  buildkit_slirp_cidr_end     = local.buildkit_slirp_cidr_start + local.buildkit_slirp_cidr_size - 1

  network_blocked_ipv4_cidr_parts = [
    for cidr in var.network_blocked_ipv4_cidrs :
    regex("^([0-9]+)\\.([0-9]+)\\.([0-9]+)\\.([0-9]+)/([0-9]+)$", cidr)
  ]
  network_blocked_ipv4_cidr_prefixes = [
    for parts in local.network_blocked_ipv4_cidr_parts :
    tonumber(parts[4])
  ]
  network_blocked_ipv4_cidr_addresses = [
    for parts in local.network_blocked_ipv4_cidr_parts :
    tonumber(parts[0]) * 16777216 + tonumber(parts[1]) * 65536 + tonumber(parts[2]) * 256 + tonumber(parts[3])
  ]
  network_blocked_ipv4_cidr_sizes = [
    for prefix in local.network_blocked_ipv4_cidr_prefixes :
    pow(2, 32 - prefix)
  ]
  network_blocked_ipv4_ranges = [
    for i, address in local.network_blocked_ipv4_cidr_addresses : {
      start = address - address % local.network_blocked_ipv4_cidr_sizes[i]
      end   = address - address % local.network_blocked_ipv4_cidr_sizes[i] + local.network_blocked_ipv4_cidr_sizes[i] - 1
    }
  ]
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

resource "aws_iam_role" "worker" {
  name = "${local.name}-worker"
  tags = var.tags

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

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
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
          for arn in [
            var.secret_arns.worker_bootstrap_token,
            var.secret_arns.checkpoint_encryption_key
          ] : arn if arn != null
        ]
      }
    ]
  })
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
    worker_bootstrap_token_secret_arn    = var.secret_arns.worker_bootstrap_token
    checkpoint_key_secret_arn            = var.secret_arns.checkpoint_encryption_key
    buildkit_service_name                = var.buildkit_service_name
    worker_service_name                  = var.worker_service_name
    worker_binary_path                   = var.worker_binary_path
    autoscaling_group_name               = local.asg_name
    launch_lifecycle_hook_name           = var.enable_lifecycle_hooks ? local.launch_hook_name : ""
    termination_lifecycle_hook_name      = var.enable_lifecycle_hooks ? local.termination_hook_name : ""
    termination_drain_timeout_seconds    = var.termination_drain_timeout_seconds
    lifecycle_heartbeat_interval_seconds = var.lifecycle_heartbeat_interval_seconds
    buildkit_slirp_cidr                  = var.buildkit_slirp_cidr
    network_blocked_ipv4_cidrs           = var.network_blocked_ipv4_cidrs
    network_blocked_ipv6_cidrs           = var.network_blocked_ipv6_cidrs
    aws_region                           = data.aws_region.current.region
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
    buildkit_slirp_cidr        = var.buildkit_slirp_cidr
    network_blocked_ipv4_cidrs = var.network_blocked_ipv4_cidrs
    reserved_env_conflicts     = local.worker_environment_conflicts
  }

  lifecycle {
    precondition {
      condition     = length(local.worker_environment_conflicts) == 0
      error_message = "worker_environment must not set infra-owned HELMR_* routing or security variables. Use explicit worker module inputs instead."
    }

    precondition {
      condition = alltrue([
        for blocked in local.network_blocked_ipv4_ranges :
        local.buildkit_slirp_cidr_start > blocked.end || blocked.start > local.buildkit_slirp_cidr_end
      ])
      error_message = "buildkit_slirp_cidr must not overlap network_blocked_ipv4_cidrs because BuildKit rootless DNS and NAT must remain reachable inside the service namespace."
    }
  }
}

resource "aws_autoscaling_group" "worker" {
  name                      = local.asg_name
  min_size                  = var.min_size
  max_size                  = var.max_size
  desired_capacity          = var.desired_capacity
  vpc_zone_identifier       = var.subnet_ids
  health_check_type         = "EC2"
  health_check_grace_period = var.health_check_grace_period_seconds
  termination_policies      = ["OldestLaunchTemplate", "OldestInstance"]

  launch_template {
    id      = aws_launch_template.worker.id
    version = "$Latest"
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

  instance_refresh {
    strategy = "Rolling"

    preferences {
      min_healthy_percentage = 50
      skip_matching          = true
    }
  }

  tag {
    key                 = "Name"
    value               = "${local.name}-worker"
    propagate_at_launch = true
  }

  lifecycle {
    precondition {
      condition     = var.min_size <= var.desired_capacity && var.desired_capacity <= var.max_size
      error_message = "worker capacity must satisfy min_size <= desired_capacity <= max_size."
    }

    precondition {
      condition     = var.termination_drain_timeout_seconds < var.termination_lifecycle_heartbeat_timeout_seconds
      error_message = "termination_drain_timeout_seconds must be less than termination_lifecycle_heartbeat_timeout_seconds."
    }
  }
}
