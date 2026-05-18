data "aws_ami" "ubuntu" {
  count       = var.parent_image == null ? 1 : 0
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

data "aws_region" "current" {}

locals {
  name                     = lower(var.name)
  parent_image             = var.parent_image == null ? data.aws_ami.ubuntu[0].id : var.parent_image
  distribution_regions     = length(var.distribution_regions) == 0 ? [data.aws_region.current.region] : var.distribution_regions
  source_bundle_object_arn = var.source_bundle_object_arn != null ? var.source_bundle_object_arn : (var.source_bundle_bucket_arn == null ? null : "${var.source_bundle_bucket_arn}/*")
  build_script = templatefile("${path.module}/templates/build-worker-image.sh.tftpl", {
    source_repository_url = var.source_repository_url
    source_ref            = var.source_ref
    source_bundle_s3_uri  = var.source_bundle_s3_uri == null ? "" : var.source_bundle_s3_uri
    buildkit_slirp_cidr   = var.buildkit_slirp_cidr
  })
}

resource "aws_iam_role" "image_builder" {
  name = "${local.name}-worker-image-builder"
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

resource "aws_iam_role_policy_attachment" "image_builder" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
    "arn:aws:iam::aws:policy/EC2InstanceProfileForImageBuilder",
    "arn:aws:iam::aws:policy/EC2InstanceProfileForImageBuilderECRContainerBuilds",
  ])

  role       = aws_iam_role.image_builder.name
  policy_arn = each.value
}

resource "aws_iam_role_policy" "source_bundle" {
  count = var.source_bundle_s3_uri == null ? 0 : 1

  name = "${local.name}-worker-image-source-bundle"
  role = aws_iam_role.image_builder.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Effect   = "Allow"
        Action   = "s3:GetObject"
        Resource = local.source_bundle_object_arn
      }
      ],
      var.source_bundle_kms_key_arn == null ? [] : [
        {
          Effect = "Allow"
          Action = [
            "kms:Decrypt"
          ]
          Resource = var.source_bundle_kms_key_arn
        }
      ]
    )
  })
}

resource "aws_iam_instance_profile" "image_builder" {
  name = "${local.name}-worker-image-builder"
  role = aws_iam_role.image_builder.name
  tags = var.tags
}

resource "aws_imagebuilder_component" "worker" {
  name     = "${local.name}-worker"
  platform = "Linux"
  version  = var.image_version

  data = yamlencode({
    schemaVersion = "1.0"
    phases = [
      {
        name = "build"
        steps = [
          {
            name   = "InstallHelmrWorker"
            action = "ExecuteBash"
            inputs = {
              commands = [local.build_script]
            }
          }
        ]
      },
      {
        name = "validate"
        steps = [
          {
            name   = "ValidateHelmrWorker"
            action = "ExecuteBash"
            inputs = {
              commands = [
                "test -x /usr/local/bin/helmr-worker",
                "test -x /usr/local/bin/firecracker",
                "test -x /usr/local/bin/jailer",
                "test -x /usr/local/bin/buildkitd",
                "test -r /var/lib/helmr/images/guest/out/vmlinuz",
                "test -r /var/lib/helmr/images/guest/out/initramfs",
                "test -r /var/lib/helmr/images/guest/out/rootfs.ext4",
                "test -r /etc/cni/conf.d/helmr.conflist",
                "systemctl cat buildkit.service >/dev/null",
                "systemctl cat helmr-worker.service >/dev/null",
              ]
            }
          }
        ]
      }
    ]
  })

  tags = var.tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_imagebuilder_image_recipe" "worker" {
  name         = "${local.name}-worker"
  parent_image = local.parent_image
  version      = var.image_version

  block_device_mapping {
    device_name = "/dev/sda1"

    ebs {
      delete_on_termination = true
      encrypted             = var.root_volume_encrypted
      volume_size           = var.root_volume_size_gb
      volume_type           = "gp3"
    }
  }

  component {
    component_arn = aws_imagebuilder_component.worker.arn
  }

  tags = var.tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_imagebuilder_infrastructure_configuration" "worker" {
  name                          = "${local.name}-worker"
  instance_profile_name         = aws_iam_instance_profile.image_builder.name
  instance_types                = var.instance_types
  subnet_id                     = var.subnet_id
  security_group_ids            = var.security_group_ids
  terminate_instance_on_failure = true

  tags = var.tags
}

resource "aws_imagebuilder_distribution_configuration" "worker" {
  name = "${local.name}-worker"

  dynamic "distribution" {
    for_each = toset(local.distribution_regions)

    content {
      region = distribution.value

      ami_distribution_configuration {
        name = "${local.name}-worker-{{ imagebuilder:buildDate }}"
        ami_tags = merge(var.tags, {
          Name                 = "${local.name}-worker"
          HelmrWorkerImageName = local.name
        })

        dynamic "launch_permission" {
          for_each = var.ami_public ? [1] : []

          content {
            user_groups = ["all"]
          }
        }
      }
    }
  }

  tags = var.tags

  lifecycle {
    precondition {
      condition     = !var.ami_public || !var.root_volume_encrypted
      error_message = "Public worker AMIs cannot contain encrypted snapshots. Set root_volume_encrypted=false when ami_public=true."
    }
  }
}

resource "aws_imagebuilder_image_pipeline" "worker" {
  name                             = "${local.name}-worker"
  image_recipe_arn                 = aws_imagebuilder_image_recipe.worker.arn
  infrastructure_configuration_arn = aws_imagebuilder_infrastructure_configuration.worker.arn
  distribution_configuration_arn   = aws_imagebuilder_distribution_configuration.worker.arn

  tags = var.tags
}

check "source_bundle_bucket" {
  assert {
    condition     = var.source_bundle_s3_uri == null || local.source_bundle_object_arn != null
    error_message = "source_bundle_object_arn is required when source_bundle_s3_uri is set."
  }
}
