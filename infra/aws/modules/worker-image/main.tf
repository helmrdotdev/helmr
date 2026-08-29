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
  name                 = lower(var.name)
  parent_image         = var.parent_image == null ? data.aws_ami.ubuntu[0].id : var.parent_image
  distribution_regions = length(var.distribution_regions) == 0 ? [data.aws_region.current.region] : sort(distinct(var.distribution_regions))
  build_script = templatefile("${path.module}/templates/build-worker-image.sh.tftpl", {
    prepare_root_script               = trimsuffix(file("${path.module}/templates/prepare-root.sh"), "\n")
    prepare_root_digest               = filesha256("${path.module}/templates/prepare-root.sh")
    host_artifacts_bundle_s3_uri      = var.host_artifacts_bundle_s3_uri
    host_artifacts_bundle_digest      = trimprefix(var.host_artifacts_bundle_digest, "sha256:")
    host_artifacts_manifest_digest    = trimprefix(var.host_artifacts_manifest_digest, "sha256:")
    runtime_artifacts_bundle_s3_uri   = var.runtime_artifacts_bundle_s3_uri
    runtime_artifacts_bundle_digest   = trimprefix(var.runtime_artifacts_bundle_digest, "sha256:")
    runtime_artifacts_manifest_digest = trimprefix(var.runtime_artifacts_manifest_digest, "sha256:")
  })
  prepare_root_digest               = filesha256("${path.module}/templates/prepare-root.sh")
  host_artifacts_manifest_digest    = trimprefix(var.host_artifacts_manifest_digest, "sha256:")
  runtime_artifacts_manifest_digest = trimprefix(var.runtime_artifacts_manifest_digest, "sha256:")
  component_document = yamlencode({
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
                "test -x /usr/local/bin/worker",
                "test -f /usr/local/sbin/helmr-prepare-root && test ! -L /usr/local/sbin/helmr-prepare-root && test -x /usr/local/sbin/helmr-prepare-root",
                "test \"$(stat -c %a /usr/local/sbin/helmr-prepare-root)\" = 755",
                "test \"$(sha256sum /usr/local/sbin/helmr-prepare-root | awk '{print $1}')\" = \"${local.prepare_root_digest}\"",
                "test -x /usr/local/bin/cpu-template-helper",
                "test -x /usr/local/bin/firecracker",
                "test -x /usr/local/bin/jailer",
                "test -f /usr/local/libexec/helmr/mkfs.ext4 && test ! -L /usr/local/libexec/helmr/mkfs.ext4 && test -x /usr/local/libexec/helmr/mkfs.ext4",
                "test \"$(stat -c %U:%G:%a /usr/local/libexec/helmr/mkfs.ext4)\" = root:root:755",
                "test -f /usr/share/helmr/mke2fs.conf && test ! -L /usr/share/helmr/mke2fs.conf",
                "test \"$(stat -c %U:%G:%a /usr/share/helmr/mke2fs.conf)\" = root:root:444",
                "test \"$(sha256sum /usr/share/helmr/worker-host-artifacts.json | awk '{print $1}')\" = \"${local.host_artifacts_manifest_digest}\"",
                "jq -e '.schema == \"helmr.worker-host-artifacts.v0\" and .arch == \"amd64\" and (keys | sort) == [\"arch\", \"files\", \"schema\"]' /usr/share/helmr/worker-host-artifacts.json >/dev/null",
                "test -r /var/lib/helmr/images/guest/out/vmlinuz",
                "test -r /var/lib/helmr/images/guest/out/initramfs",
                "test -r /var/lib/helmr/images/guest/out/rootfs.squashfs",
                "test -r /var/lib/helmr/images/guest/out/runtime-artifacts.json",
                "test \"$(stat -c %U:%G:%a /var/lib/helmr/images/guest/out/vmlinuz)\" = root:root:444",
                "test \"$(stat -c %U:%G:%a /var/lib/helmr/images/guest/out/initramfs)\" = root:root:444",
                "test \"$(stat -c %U:%G:%a /var/lib/helmr/images/guest/out/rootfs.squashfs)\" = root:root:444",
                "test \"$(stat -c %U:%G:%a /var/lib/helmr/images/guest/out/runtime-artifacts.json)\" = root:root:444",
                "test \"$(sha256sum /var/lib/helmr/images/guest/out/runtime-artifacts.json | awk '{print $1}')\" = \"${local.runtime_artifacts_manifest_digest}\"",
                "cd /var/lib/helmr/images/guest/out && jq -e '.schema == \"helmr.runtime-artifacts.v0\" and .arch == \"amd64\" and .vm_runtime_contract == \"helmr.vm-runtime.v0\"' runtime-artifacts.json >/dev/null && test \"$(sha256sum vmlinuz | awk '{print $1}')\" = \"$(jq -r .kernel.digest runtime-artifacts.json | sed 's/^sha256://')\" && test \"$(sha256sum initramfs | awk '{print $1}')\" = \"$(jq -r .initramfs.digest runtime-artifacts.json | sed 's/^sha256://')\" && test \"$(sha256sum rootfs.squashfs | awk '{print $1}')\" = \"$(jq -r .rootfs.digest runtime-artifacts.json | sed 's/^sha256://')\" && test \"$(stat -c %s vmlinuz)\" = \"$(jq -r .kernel.size_bytes runtime-artifacts.json)\" && test \"$(stat -c %s initramfs)\" = \"$(jq -r .initramfs.size_bytes runtime-artifacts.json)\" && test \"$(stat -c %s rootfs.squashfs)\" = \"$(jq -r .rootfs.size_bytes runtime-artifacts.json)\"",
                "command -v blockdev fallocate findmnt growpart losetup lsblk mountpoint blkid readlink resize2fs >/dev/null",
                "command -v aws curl gpgv ip mksquashfs nft patchelf xz >/dev/null",
                "mksquashfs -version 2>&1 | head -n 1 | grep -F 'mksquashfs version 4.6.1 '",
                "aws --version 2>&1 | grep -F 'aws-cli/2.'",
                "! command -v bun >/dev/null",
                "! command -v docker >/dev/null",
                "! command -v go >/dev/null",
                "! command -v nix >/dev/null",
                "test ! -e /opt/helmr-src",
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
  component_definition = {
    schema   = "helmr.worker-image-component-definition.v0"
    document = local.component_document
  }
  component_definition_digest = "sha256:${sha256(jsonencode(local.component_definition))}"
  component_name              = "${local.name}-worker-component-${trimprefix(local.component_definition_digest, "sha256:")}"
  root_block_device_mapping = {
    deviceName = "/dev/sda1"
    ebs = {
      deleteOnTermination = true
      encrypted           = var.root_volume_encrypted
      volumeSize          = var.root_volume_size_gb
      volumeType          = "gp3"
    }
  }
  image_definition = {
    schema                    = "helmr.worker-image-definition.v0"
    componentDefinitionDigest = local.component_definition_digest
    parentImage               = local.parent_image
    blockDeviceMappings       = [local.root_block_device_mapping]
  }
  image_definition_digest = "sha256:${sha256(jsonencode(local.image_definition))}"
  image_recipe_name       = "${local.name}-worker-recipe-${trimprefix(local.image_definition_digest, "sha256:")}"
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

resource "aws_iam_role_policy" "build_artifacts" {
  name = "${local.name}-worker-image-build-artifacts"
  role = aws_iam_role.image_builder.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Effect = "Allow"
        Action = "s3:GetObject"
        Resource = [
          var.host_artifacts_bundle_object_arn,
          var.runtime_artifacts_bundle_object_arn,
        ]
      }
      ],
      length(compact([
        var.host_artifacts_bundle_kms_key_arn,
        var.runtime_artifacts_bundle_kms_key_arn,
        ])) == 0 ? [] : [
        {
          Effect = "Allow"
          Action = [
            "kms:Decrypt"
          ]
          Resource = distinct(compact([
            var.host_artifacts_bundle_kms_key_arn,
            var.runtime_artifacts_bundle_kms_key_arn,
          ]))
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
  name     = local.component_name
  platform = "Linux"
  version  = "1.0.0"
  data     = local.component_document

  tags = merge(var.tags, {
    HelmrComponentDefinitionDigest = local.component_definition_digest
  })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_imagebuilder_image_recipe" "worker" {
  name         = local.image_recipe_name
  parent_image = local.parent_image
  version      = "1.0.0"

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

  tags = merge(var.tags, {
    HelmrComponentDefinitionDigest = local.component_definition_digest
    HelmrImageDefinitionDigest     = local.image_definition_digest
    HelmrResolvedParentImageID     = local.parent_image
  })

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
        ami_tags = merge(
          var.tags,
          {
            Name                           = "${local.name}-worker"
            HelmrWorkerImageName           = local.name
            HelmrComponentDefinitionDigest = local.component_definition_digest
            HelmrImageDefinitionDigest     = local.image_definition_digest
            HelmrResolvedParentImageID     = local.parent_image
            HelmrPrepareRootDigest         = "sha256:${local.prepare_root_digest}"
            HelmrHostBundleDigest          = var.host_artifacts_bundle_digest
            HelmrHostArtifactsDigest       = var.host_artifacts_manifest_digest
            HelmrRuntimeBundleDigest       = var.runtime_artifacts_bundle_digest
            HelmrRuntimeArtifactsDigest    = var.runtime_artifacts_manifest_digest
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
