mock_provider "aws" {}

variables {
  name = "helmr-test"
}

run "disabled_omitted_principals" {
  command = apply
  variables {
    create_platform_publisher = false
  }
  assert {
    condition = (
      length(aws_iam_role.platform_publisher) == 0 &&
      length(aws_iam_role_policy.platform_publisher) == 0 &&
      output.platform_publisher_role_arn == null
    )
    error_message = "Disabled publisher must require no principals and create neither IAM resource."
  }
  assert {
    condition = (
      output.platform_store_bucket_arn == aws_s3_bucket.platform_store.arn &&
      output.platform_store_kms_key_arn == aws_kms_key.platform_store.arn &&
      output.controlplane_release_repository_arn == aws_ecr_repository.controlplane_releases.arn &&
      output.release_artifact_bucket_arn == aws_s3_bucket.release_artifacts.arn
    )
    error_message = "Disabling the publisher must preserve artifact stores and encryption resources."
  }
}

run "default_enabled_requires_principals" {
  command         = plan
  expect_failures = [var.platform_publisher_principal_arns]
}

run "enabled_invalid_principal" {
  command = plan
  variables {
    create_platform_publisher         = true
    platform_publisher_principal_arns = ["arn:aws:iam::000000000000:root"]
  }
  expect_failures = [var.platform_publisher_principal_arns]
}

run "explicit_enabled" {
  command = apply
  variables {
    create_platform_publisher         = true
    platform_publisher_principal_arns = ["arn:aws:iam::000000000000:role/publisher"]
  }
  assert {
    condition = (
      length(aws_iam_role.platform_publisher) == 1 &&
      length(aws_iam_role_policy.platform_publisher) == 1 &&
      aws_iam_role.platform_publisher[0].name == "helmr-test-platform-publisher" &&
      aws_iam_role_policy.platform_publisher[0].role == aws_iam_role.platform_publisher[0].id &&
      jsondecode(aws_iam_role.platform_publisher[0].assume_role_policy).Statement[0].Principal.AWS[0] == "arn:aws:iam::000000000000:role/publisher" &&
      output.platform_publisher_role_arn == aws_iam_role.platform_publisher[0].arn
    )
    error_message = "Enabled publisher must preserve its role name, trust, policy attachment and output."
  }
}

run "disable_existing_publisher_preserves_stores" {
  command = apply
  variables {
    create_platform_publisher = false
  }
  assert {
    condition = (
      length(aws_iam_role.platform_publisher) == 0 &&
      length(aws_iam_role_policy.platform_publisher) == 0 &&
      output.platform_publisher_role_arn == null
    )
    error_message = "Removing an enabled publisher must retain the exact existing artifact stores and keys."
  }
}
