mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_resource "aws_imagebuilder_component" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:component/helmr-test-worker/1.0.0/1"
    }
  }

  mock_resource "aws_imagebuilder_image_recipe" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:image-recipe/helmr-test-worker/1.0.0"
    }
  }

  mock_resource "aws_imagebuilder_infrastructure_configuration" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:infrastructure-configuration/helmr-test-worker"
    }
  }

  mock_resource "aws_imagebuilder_distribution_configuration" {
    defaults = {
      arn = "arn:aws:imagebuilder:us-east-1:000000000000:distribution-configuration/helmr-test-worker"
    }
  }
}

variables {
  name                                = "helmr-test"
  parent_image                        = "ami-00000000000000000"
  root_volume_encrypted               = false
  host_artifacts_bundle_s3_uri        = "s3://helmr-test/host/worker-host.tar"
  host_artifacts_bundle_object_arn    = "arn:aws:s3:::helmr-test/host/worker-host.tar"
  host_artifacts_bundle_digest        = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
  host_artifacts_manifest_digest      = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
  runtime_artifacts_bundle_s3_uri     = "s3://helmr-test/runtime/worker-runtime.tar"
  runtime_artifacts_bundle_object_arn = "arn:aws:s3:::helmr-test/runtime/worker-runtime.tar"
  runtime_artifacts_bundle_digest     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  runtime_artifacts_manifest_digest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

run "image_installs_verified_worker_artifacts" {
  command = plan

  assert {
    condition = (
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/bin/helmr-worker") &&
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/bin/cpu-template-helper") &&
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/bin/firecracker") &&
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/libexec/helmr/mkfs.ext4") &&
      strcontains(aws_imagebuilder_component.worker.data, "/usr/share/helmr/mke2fs.conf") &&
      strcontains(aws_imagebuilder_component.worker.data, "root:root:755") &&
      strcontains(aws_imagebuilder_component.worker.data, "gpgv") &&
      strcontains(aws_imagebuilder_component.worker.data, "mksquashfs") &&
      strcontains(aws_imagebuilder_component.worker.data, "cloud-guest-utils") &&
      strcontains(aws_imagebuilder_component.worker.data, "command -v blockdev fallocate findmnt growpart losetup lsblk mountpoint blkid") &&
      strcontains(aws_imagebuilder_component.worker.data, "readlink resize2fs >/dev/null") &&
      strcontains(aws_imagebuilder_component.worker.data, "aws_cli_version=2.31.39") &&
      strcontains(aws_imagebuilder_component.worker.data, "https://awscli.amazonaws.com/awscli-exe-linux-x86_64-") &&
      strcontains(aws_imagebuilder_component.worker.data, "5a2ad4e63f8f687d735f8e7a132b3622a1cf08fa884c53e3423c9b83a3c0d663")
    )
    error_message = "Worker AMI must contain the execution host and runtime verification tools."
  }

  assert {
    condition = (
      strcontains(aws_imagebuilder_component.worker.data, "/usr/local/sbin/helmr-prepare-root") &&
      strcontains(aws_imagebuilder_component.worker.data, "usage: helmr-prepare-root EXPECTED_DEVICE_BYTES") &&
      strcontains(aws_imagebuilder_component.worker.data, filesha256("${path.module}/templates/prepare-root.sh")) &&
      strcontains(aws_imagebuilder_component.worker.data, "stat -c %a /usr/local/sbin/helmr-prepare-root") &&
      strcontains(local.build_script, "<<'HELMR_PREPARE_ROOT'\n${file("${path.module}/templates/prepare-root.sh")}HELMR_PREPARE_ROOT") &&
      one(
        one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
      ).ami_tags.HelmrPrepareRootDigest == "sha256:${filesha256("${path.module}/templates/prepare-root.sh")}"
    )
    error_message = "Worker AMIs must install the exact checked-in root preparation helper as an executable, source-bound image artifact."
  }

  assert {
    condition = (
      strcontains(aws_imagebuilder_component.worker.data, "s3://helmr-test/runtime/worker-runtime.tar") &&
      strcontains(aws_imagebuilder_component.worker.data, "s3://helmr-test/host/worker-host.tar") &&
      strcontains(aws_imagebuilder_component.worker.data, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd") &&
      strcontains(aws_imagebuilder_component.worker.data, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc") &&
      strcontains(aws_imagebuilder_component.worker.data, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") &&
      strcontains(aws_imagebuilder_component.worker.data, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") &&
      strcontains(aws_imagebuilder_component.worker.data, "install -m 0444 \"$work/runtime/$name\"") &&
      strcontains(aws_imagebuilder_component.worker.data, "stat -c %U:%G:%a /var/lib/helmr/images/guest/out/vmlinuz") &&
      strcontains(aws_imagebuilder_component.worker.data, "stat -c %U:%G:%a /var/lib/helmr/images/guest/out/initramfs") &&
      strcontains(aws_imagebuilder_component.worker.data, "stat -c %U:%G:%a /var/lib/helmr/images/guest/out/rootfs.squashfs") &&
      strcontains(aws_imagebuilder_component.worker.data, "stat -c %U:%G:%a /var/lib/helmr/images/guest/out/runtime-artifacts.json") &&
      length(regexall("root:root:444", aws_imagebuilder_component.worker.data)) == 5
    )
    error_message = "Worker image build must install and verify the exact immutable, jailed-VMM-readable runtime artifact bundle without rebuilding it."
  }

  assert {
    condition = (
      local.component_definition == {
        schema   = "helmr.worker-image-component-definition.v0"
        document = aws_imagebuilder_component.worker.data
      } &&
      local.component_definition_digest == "sha256:${sha256(jsonencode(local.component_definition))}" &&
      aws_imagebuilder_component.worker.name == "helmr-test-worker-component-${trimprefix(local.component_definition_digest, "sha256:")}" &&
      aws_imagebuilder_component.worker.version == "1.0.0" &&
      length(aws_imagebuilder_component.worker.name) <= 128
    )
    error_message = "The complete rendered component definition must have a full-digest resource name and fixed incidental version."
  }

  assert {
    condition = (
      aws_imagebuilder_image_recipe.worker.name == "helmr-test-worker-recipe-${trimprefix(local.image_definition_digest, "sha256:")}" &&
      aws_imagebuilder_image_recipe.worker.version == "1.0.0" &&
      length(aws_imagebuilder_image_recipe.worker.name) <= 128 &&
      output.resolved_parent_image_id == "ami-00000000000000000" &&
      output.root_block_device_mapping == {
        deviceName = "/dev/sda1"
        ebs = {
          deleteOnTermination = true
          encrypted           = false
          volumeSize          = 24
          volumeType          = "gp3"
        }
      }
    )
    error_message = "The exact parent, component digest, and complete root mapping must identify the image recipe."
  }

  assert {
    condition = (
      one(
        one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
      ).ami_tags.HelmrComponentDefinitionDigest == local.component_definition_digest &&
      one(
        one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
      ).ami_tags.HelmrImageDefinitionDigest == local.image_definition_digest &&
      one(
        one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
      ).ami_tags.HelmrResolvedParentImageID == local.parent_image
    )
    error_message = "Worker AMI tags must bind the exact component, recipe, and resolved parent definitions."
  }

  assert {
    condition = one(
      one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
    ).ami_tags.HelmrHostArtifactsDigest == "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    error_message = "Worker image build and AMI provenance must bind the exact host artifact manifest digest."
  }

  assert {
    condition = one(
      one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
    ).ami_tags.HelmrHostBundleDigest == "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    error_message = "Worker AMI provenance must bind the exact host artifact bundle digest."
  }

  assert {
    condition = one(
      one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
    ).ami_tags.HelmrRuntimeArtifactsDigest == "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    error_message = "Worker image build and AMI provenance must bind the exact runtime artifact manifest digest."
  }

  assert {
    condition = one(
      one(aws_imagebuilder_distribution_configuration.worker.distribution).ami_distribution_configuration
    ).ami_tags.HelmrRuntimeBundleDigest == "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    error_message = "Worker AMI provenance must bind the exact runtime artifact bundle digest."
  }

  assert {
    condition = (
      strcontains(aws_iam_role_policy.build_artifacts.policy, "arn:aws:s3:::helmr-test/host/worker-host.tar") &&
      strcontains(aws_iam_role_policy.build_artifacts.policy, "arn:aws:s3:::helmr-test/runtime/worker-runtime.tar")
    )
    error_message = "Image Builder must only receive access to the exact host and runtime artifact bundle objects."
  }
}

run "changed_host_artifact_changes_definitions" {
  command = plan

  variables {
    host_artifacts_bundle_digest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
  }

  assert {
    condition = (
      local.component_definition_digest != "sha256:${sha256(jsonencode({
        schema = "helmr.worker-image-component-definition.v0"
        document = replace(
          local.component_document,
          "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
          "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
        )
      }))}" &&
      local.image_definition.componentDefinitionDigest == local.component_definition_digest
    )
    error_message = "A changed component input must change both definition digests."
  }
}

run "changed_parent_changes_only_image_definition" {
  command = plan

  variables {
    parent_image = "ami-11111111111111111"
  }

  assert {
    condition = (
      local.image_definition_digest != "sha256:${sha256(jsonencode(merge(
        local.image_definition,
        { parentImage = "ami-00000000000000000" }
      )))}" &&
      local.image_definition.parentImage == "ami-11111111111111111"
    )
    error_message = "A changed resolved parent AMI must change only the image definition."
  }
}

run "distribution_policy_does_not_change_image_definition" {
  command = plan

  variables {
    ami_public           = true
    distribution_regions = ["us-east-1", "us-west-2"]
  }

  assert {
    condition = (
      keys(local.image_definition) == ["blockDeviceMappings", "componentDefinitionDigest", "parentImage", "schema"] &&
      local.image_definition.schema == "helmr.worker-image-definition.v0" &&
      local.image_definition.componentDefinitionDigest == local.component_definition_digest &&
      local.image_definition.parentImage == local.parent_image &&
      local.image_definition.blockDeviceMappings == [local.root_block_device_mapping] &&
      toset(output.distribution_regions) == toset(["us-east-1", "us-west-2"]) &&
      output.ami_public
    )
    error_message = "Region and visibility policy must not be artifact or image-definition identity."
  }
}
