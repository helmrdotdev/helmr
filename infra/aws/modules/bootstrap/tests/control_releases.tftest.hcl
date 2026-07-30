mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "000000000000"
    }
  }
}

override_resource {
  target = aws_kms_key.control_releases
  values = {
    arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000003"
  }
}

variables {
  name = "helmr-test"
  platform_publisher_principal_arns = [
    "arn:aws:iam::000000000000:role/helmr-installation-publisher"
  ]
}

run "control_release_repository_is_durable_and_immutable" {
  command = apply

  assert {
    condition = (
      aws_ecr_repository.control_releases.name == "helmr-test/control-releases" &&
      aws_ecr_repository.control_releases.image_tag_mutability == "IMMUTABLE" &&
      !aws_ecr_repository.control_releases.force_delete &&
      aws_ecr_repository.control_releases.image_scanning_configuration[0].scan_on_push &&
      aws_ecr_repository.control_releases.encryption_configuration[0].encryption_type == "KMS" &&
      aws_ecr_repository.control_releases.encryption_configuration[0].kms_key == aws_kms_key.control_releases.arn
    )
    error_message = "Control release images must live in one immutable, scan-enabled, KMS-encrypted, non-force-deletable repository."
  }

  assert {
    condition = (
      output.control_release_repository_url == aws_ecr_repository.control_releases.repository_url &&
      output.control_release_repository_name == aws_ecr_repository.control_releases.name &&
      output.control_release_repository_arn == aws_ecr_repository.control_releases.arn &&
      output.control_release_kms_key_arn == aws_kms_key.control_releases.arn
    )
    error_message = "bootstrap must expose the exact durable Control release repository and KMS authority."
  }
}

run "release_publisher_cannot_delete_control_images" {
  command = apply

  assert {
    condition = (
      strcontains(aws_iam_role_policy.platform_publisher.policy, aws_ecr_repository.control_releases.arn) &&
      strcontains(aws_iam_role_policy.platform_publisher.policy, "ecr:PutImage") &&
      strcontains(aws_iam_role_policy.platform_publisher.policy, "ecr:BatchGetImage") &&
      strcontains(aws_iam_role_policy.platform_publisher.policy, "ecr:DescribeImages") &&
      !strcontains(aws_iam_role_policy.platform_publisher.policy, "ecr:GetDownloadUrlForLayer") &&
      !strcontains(aws_iam_role_policy.platform_publisher.policy, "ecr:BatchDeleteImage") &&
      !strcontains(aws_iam_role_policy.platform_publisher.policy, "ecr:DeleteRepository") &&
      !strcontains(aws_iam_role_policy.platform_publisher.policy, "ecr:SetRepositoryPolicy")
    )
    error_message = "release publisher authority must stop at publish and verification."
  }
}
