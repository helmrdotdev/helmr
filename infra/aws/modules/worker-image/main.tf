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

data "aws_partition" "current" {}

locals {
  name                    = lower(var.name)
  parent_image            = var.parent_image == null ? data.aws_ami.ubuntu[0].id : var.parent_image
  distribution_regions    = length(var.distribution_regions) == 0 ? [data.aws_region.current.region] : var.distribution_regions
  create_instance_profile = var.instance_profile_name == null
  instance_profile_name   = local.create_instance_profile ? aws_iam_instance_profile.image_builder[0].name : var.instance_profile_name
  release_package_parts   = try(regex("^s3://([^/]+)/(.+)$", var.release_package_s3_uri), ["", ""])
  release_package_bucket  = local.release_package_parts[0]
  release_package_key     = local.release_package_parts[1]
  release_package_arn     = "arn:${data.aws_partition.current.partition}:s3:::${local.release_package_bucket}/${local.release_package_key}"
  build_script = templatefile("${path.module}/templates/build-worker-image.sh.tftpl", {
    source_repository_url       = var.source_repository_url
    source_ref                  = var.source_ref
    source_bundle_s3_uri        = var.source_bundle_s3_uri == null ? "" : var.source_bundle_s3_uri
    buildkit_slirp_cidr         = var.buildkit_slirp_cidr
    release_trust_mode          = var.release_trust_mode
    release_trust_san           = var.release_trust_san == null ? "" : var.release_trust_san
    release_trust_source_digest = var.release_trust_source_digest == null ? "" : var.release_trust_source_digest
  })
  install_release_script = templatefile("${path.module}/templates/install-runtime-release.sh.tftpl", {
    release_package_bucket     = local.release_package_bucket
    release_package_key        = local.release_package_key
    release_package_version_id = var.release_package_version_id
    release_package_sha256     = var.release_package_sha256
    release_validator          = file("${path.module}/templates/runtime-release.py")
  })
  validate_release_script = templatefile("${path.module}/templates/validate-runtime-release.sh.tftpl", {
    release_package_bucket     = local.release_package_bucket
    release_package_key        = local.release_package_key
    release_package_version_id = var.release_package_version_id
    release_package_sha256     = var.release_package_sha256
  })
}

check "closed_release_trust" {
  assert {
    condition = (
      var.release_trust_mode == "production" &&
      var.release_trust_san == null &&
      var.release_trust_source_digest == null &&
      var.release_provenance_sha256 == null
      ) || (
      var.release_trust_mode == "development" &&
      var.release_trust_san != null &&
      var.release_trust_source_digest != null &&
      var.release_provenance_sha256 != null &&
      var.source_ref == var.release_trust_source_digest
    )
    error_message = "production trust forbids dev provenance inputs; development trust requires exact SAN/source/provenance inputs and source_ref equality."
  }
}

resource "aws_iam_role" "image_builder" {
  count = local.create_instance_profile ? 1 : 0

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
  for_each = local.create_instance_profile ? toset([
    "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
    "arn:aws:iam::aws:policy/EC2InstanceProfileForImageBuilder",
    "arn:aws:iam::aws:policy/EC2InstanceProfileForImageBuilderECRContainerBuilds",
  ]) : toset([])

  role       = aws_iam_role.image_builder[0].name
  policy_arn = each.value
}

resource "aws_iam_role_policy" "source_bundle" {
  count = local.create_instance_profile && var.source_bundle_s3_uri != null ? 1 : 0

  name = "${local.name}-worker-image-source-bundle"
  role = aws_iam_role.image_builder[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Effect   = "Allow"
        Action   = "s3:GetObject"
        Resource = var.source_bundle_object_arn
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

  lifecycle {
    precondition {
      condition     = var.source_bundle_object_arn != null
      error_message = "source_bundle_object_arn is required when source_bundle_s3_uri is set."
    }
  }
}

resource "aws_iam_role_policy" "release_package" {
  count = local.create_instance_profile ? 1 : 0

  name = "${local.name}-worker-image-release-package"
  role = aws_iam_role.image_builder[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "ReadExactReleasePackageVersion"
        Effect   = "Allow"
        Action   = "s3:GetObjectVersion"
        Resource = var.release_package_object_arn
        Condition = {
          StringEquals = {
            "s3:VersionId" = var.release_package_version_id
          }
        }
      }
      ],
      var.release_package_kms_key_arn == null ? [] : [
        {
          Sid      = "DecryptReleasePackage"
          Effect   = "Allow"
          Action   = "kms:Decrypt"
          Resource = var.release_package_kms_key_arn
        }
      ]
    )
  })
}

resource "aws_iam_instance_profile" "image_builder" {
  count = local.create_instance_profile ? 1 : 0

  name = "${local.name}-worker-image-builder"
  role = aws_iam_role.image_builder[0].name
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
                "test -r /var/lib/helmr/images/guest/out/runtime-artifacts.json",
                "test \"$(find /usr/lib/helmr/manager-release/objects/sha256 -mindepth 1 -maxdepth 1 -type f | wc -l)\" -ge 2",
                "cd /var/lib/helmr/images/guest/out && jq -e '.schema == \"helmr.runtime-artifacts.v0\" and .arch == \"amd64\" and .runtime_abi == \"helmr.firecracker.snapshot.v0\"' runtime-artifacts.json >/dev/null && test \"$(sha256sum vmlinuz | awk '{print $1}')\" = \"$(jq -r .kernel.digest runtime-artifacts.json | sed 's/^sha256://')\" && test \"$(sha256sum initramfs | awk '{print $1}')\" = \"$(jq -r .initramfs.digest runtime-artifacts.json | sed 's/^sha256://')\" && test \"$(sha256sum rootfs.ext4 | awk '{print $1}')\" = \"$(jq -r .rootfs.digest runtime-artifacts.json | sed 's/^sha256://')\" && test \"$(stat -c %s vmlinuz)\" = \"$(jq -r .kernel.size_bytes runtime-artifacts.json)\" && test \"$(stat -c %s initramfs)\" = \"$(jq -r .initramfs.size_bytes runtime-artifacts.json)\" && test \"$(stat -c %s rootfs.ext4)\" = \"$(jq -r .rootfs.size_bytes runtime-artifacts.json)\"",
                "test -r /etc/cni/conf.d/helmr.conflist",
                "command -v fallocate findmnt losetup mountpoint blkid mkfs.ext4 >/dev/null",
                "systemctl cat helmr-buildkit.service >/dev/null",
                "systemctl cat helmr-worker.service >/dev/null",
                "systemd-analyze verify /etc/systemd/system/helmr-worker.service",
                "test \"$(systemctl show helmr-worker.service -p Delegate --value)\" = yes",
                "test \"$(systemctl show helmr-worker.service -p DelegateSubgroup --value)\" = supervisor",
                "test \"$(systemctl show helmr-worker.service -p KillMode --value)\" = mixed",
                "test \"$(systemctl show helmr-worker.service -p TasksMax --value)\" = infinity",
                "getent passwd helmr-verifier >/dev/null",
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

resource "aws_imagebuilder_component" "runtime_release" {
  name     = "${local.name}-worker-runtime-release"
  platform = "Linux"
  version  = var.image_version

  data = yamlencode({
    schemaVersion = "1.0"
    phases = [
      {
        name = "build"
        steps = [
          {
            name   = "InstallRuntimeRelease"
            action = "ExecuteBash"
            inputs = {
              commands = [local.install_release_script]
            }
          }
        ]
      }
    ]
  })

  tags = var.tags

  lifecycle {
    create_before_destroy = true

    precondition {
      condition     = var.release_package_object_arn == local.release_package_arn
      error_message = "release_package_object_arn must identify the exact object in release_package_s3_uri."
    }
  }
}

resource "aws_imagebuilder_component" "runtime_release_validation" {
  name     = "${local.name}-worker-runtime-release-validation"
  platform = "Linux"
  version  = var.image_version

  data = yamlencode({
    schemaVersion = "1.0"
    phases = [
      {
        name = "validate"
        steps = [
          {
            name   = "ValidateRuntimeRelease"
            action = "ExecuteBash"
            inputs = {
              commands = [local.validate_release_script]
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

  component {
    component_arn = aws_imagebuilder_component.runtime_release.arn
  }

  component {
    component_arn = aws_imagebuilder_component.runtime_release_validation.arn
  }

  tags = var.tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_imagebuilder_infrastructure_configuration" "worker" {
  name                          = "${local.name}-worker"
  instance_profile_name         = local.instance_profile_name
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
        ami_tags = merge(
          var.tags,
          {
            Name                             = "${local.name}-worker"
            HelmrWorkerImageName             = local.name
            HelmrReleasePackageSHA256        = var.release_package_sha256
            HelmrReleasePackageVersionSHA256 = sha256(var.release_package_version_id)
            HelmrReleaseTrustMode            = var.release_trust_mode
            HelmrSourceCommit                = var.source_ref
          },
          var.release_provenance_sha256 == null ? {} : {
            HelmrDevReleaseProvenanceSHA256 = var.release_provenance_sha256
          },
          var.release_trust_san == null ? {} : {
            HelmrReleaseTrustSANHash = sha256(var.release_trust_san)
          },
        )

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
