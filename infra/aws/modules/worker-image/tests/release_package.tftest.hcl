mock_provider "aws" {
  mock_data "aws_ami" {
    defaults = {
      id = "ami-00000000000000000"
    }
  }

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
}

override_resource {
  target = aws_imagebuilder_component.worker
  values = {
    arn = "arn:aws:imagebuilder:us-east-1:000000000000:component/helmr-test-worker/0.0.0/1"
  }
}

override_resource {
  target = aws_imagebuilder_component.runtime_release
  values = {
    arn = "arn:aws:imagebuilder:us-east-1:000000000000:component/helmr-test-worker-runtime-release/0.0.0/1"
  }
}

override_resource {
  target = aws_imagebuilder_component.runtime_release_validation
  values = {
    arn = "arn:aws:imagebuilder:us-east-1:000000000000:component/helmr-test-worker-runtime-release-validation/0.0.0/1"
  }
}

override_resource {
  target = aws_imagebuilder_image_recipe.worker
  values = {
    arn = "arn:aws:imagebuilder:us-east-1:000000000000:image-recipe/helmr-test-worker/0.0.0"
  }
}

override_resource {
  target = aws_imagebuilder_infrastructure_configuration.worker
  values = {
    arn = "arn:aws:imagebuilder:us-east-1:000000000000:infrastructure-configuration/helmr-test-worker"
  }
}

override_resource {
  target = aws_imagebuilder_distribution_configuration.worker
  values = {
    arn = "arn:aws:imagebuilder:us-east-1:000000000000:distribution-configuration/helmr-test-worker"
  }
}

variables {
  name                        = "helmr-test"
  release_package_s3_uri      = "s3://helmr-release/worker/v0/x86_64/runtime-release.tar"
  release_package_object_arn  = "arn:aws:s3:::helmr-release/worker/v0/x86_64/runtime-release.tar"
  release_package_version_id  = "3/L4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY"
  release_package_sha256      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  release_package_kms_key_arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000000"
}

run "release_package_transport_is_version_pinned" {
  command = plan

  assert {
    condition = (
      jsondecode(aws_iam_role_policy.release_package[0].policy).Statement[0].Action == "s3:GetObjectVersion" &&
      jsondecode(aws_iam_role_policy.release_package[0].policy).Statement[0].Resource == var.release_package_object_arn &&
      jsondecode(aws_iam_role_policy.release_package[0].policy).Statement[0].Condition.StringEquals["s3:VersionId"] == var.release_package_version_id &&
      jsondecode(aws_iam_role_policy.release_package[0].policy).Statement[1].Action == "kms:Decrypt" &&
      jsondecode(aws_iam_role_policy.release_package[0].policy).Statement[1].Resource == var.release_package_kms_key_arn
    )
    error_message = "Image Builder must read only the exact release-package object version and optional KMS key."
  }

  assert {
    condition = (
      strcontains(aws_imagebuilder_component.runtime_release.data, "s3api get-object") &&
      strcontains(aws_imagebuilder_component.runtime_release.data, "--version-id '${var.release_package_version_id}'") &&
      strcontains(aws_imagebuilder_component.runtime_release.data, var.release_package_sha256) &&
      strcontains(aws_imagebuilder_component.runtime_release.data, "tarfile.open(archive, mode=\"r:\")") &&
      strcontains(aws_imagebuilder_component.runtime_release.data, "/usr/lib/helmr/runtime-release") &&
      strcontains(aws_imagebuilder_component.runtime_release.data, "/usr/lib/helmr/toolchain-release") &&
      strcontains(aws_imagebuilder_component.runtime_release.data, "toolchain-release/objects/sha256") &&
      strcontains(aws_imagebuilder_component.runtime_release.data, "install -o root -g root -m 0444") &&
      strcontains(aws_imagebuilder_component.runtime_release.data, "sha256sum --check --strict") &&
      strcontains(aws_imagebuilder_component.runtime_release_validation.data, "s3api get-object") &&
      strcontains(aws_imagebuilder_component.runtime_release_validation.data, "--version-id '${var.release_package_version_id}'") &&
      strcontains(aws_imagebuilder_component.runtime_release_validation.data, var.release_package_sha256) &&
      strcontains(aws_imagebuilder_component.runtime_release_validation.data, "sha256sum \"$path\"") &&
      strcontains(aws_imagebuilder_component.runtime_release_validation.data, "toolchain-release/objects/sha256") &&
      length(aws_imagebuilder_image_recipe.worker.component) == 3
    )
    error_message = "The image component must authenticate, safely unpack, install, and validate the exact release package."
  }
}

run "release_package_object_identity_must_match_uri" {
  command = plan

  variables {
    release_package_object_arn = "arn:aws:s3:::other-bucket/worker/v0/x86_64/runtime-release.tar"
  }

  expect_failures = [aws_imagebuilder_component.runtime_release]
}

run "release_package_kms_permission_is_optional" {
  command = plan

  variables {
    release_package_kms_key_arn = null
  }

  assert {
    condition     = length(jsondecode(aws_iam_role_policy.release_package[0].policy).Statement) == 1
    error_message = "An unencrypted or S3-managed release package must not grant KMS permission."
  }
}

run "release_package_digest_must_be_lowercase" {
  command = plan

  variables {
    release_package_sha256 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
  }

  expect_failures = [var.release_package_sha256]
}

run "release_package_requires_a_real_version" {
  command = plan

  variables {
    release_package_version_id = "null"
  }

  expect_failures = [var.release_package_version_id]
}

run "release_package_object_cannot_expand_iam_authority" {
  command = plan

  variables {
    release_package_s3_uri     = "s3://helmr-release/worker/v0/*/runtime-release.tar"
    release_package_object_arn = "arn:aws:s3:::helmr-release/worker/v0/*/runtime-release.tar"
  }

  expect_failures = [
    var.release_package_s3_uri,
    var.release_package_object_arn,
  ]
}
