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
  target = aws_kms_key.source_artifacts
  values = {
    arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000001"
  }
}

override_resource {
  target = aws_kms_key.runtime_store
  values = {
    arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000002"
  }
}

variables {
  name = "helmr-test"
  runtime_provisioner_principal_arns = [
    "arn:aws:iam::000000000000:role/helmr-installation-provisioner"
  ]
  runtime_rollout_orchestrator_principal_arns = [
    "arn:aws:iam::000000000000:role/helmr-installation-rollout"
  ]
}

run "runtime_store_is_versioned_non_expiring_and_separate" {
  command = apply

  assert {
    condition     = aws_s3_bucket.runtime_store.bucket != aws_s3_bucket.source_artifacts.bucket && aws_kms_key.runtime_store.arn != aws_kms_key.source_artifacts.arn
    error_message = "managed runtime must use a dedicated bucket and KMS key."
  }

  assert {
    condition     = aws_s3_bucket_versioning.runtime_store.versioning_configuration[0].status == "Enabled"
    error_message = "managed runtime store must have versioning enabled."
  }

  assert {
    condition     = aws_s3_bucket.runtime_store.force_destroy
    error_message = "explicit whole-installation teardown must be able to empty all versions and delete markers after policy guards are removed."
  }

  assert {
    condition = (
      output.runtime_store_uri == "s3://${aws_s3_bucket.runtime_store.bucket}/objects" &&
      output.runtime_store_bucket_arn == aws_s3_bucket.runtime_store.arn &&
      output.runtime_store_kms_key_arn == aws_kms_key.runtime_store.arn
    )
    error_message = "bootstrap outputs must expose the fixed /objects URI and its physical bucket/KMS authority."
  }
}

run "runtime_store_roles_are_prefix_bounded" {
  command = apply

  assert {
    condition = (
      strcontains(aws_iam_role_policy.runtime_provisioner.policy, "${aws_s3_bucket.runtime_store.arn}/objects/sha256/*") &&
      !strcontains(aws_iam_role_policy.runtime_provisioner.policy, "/control/runtime/") &&
      strcontains(aws_iam_role_policy.runtime_rollout_orchestrator.policy, "${aws_s3_bucket.runtime_store.arn}/control/runtime/*") &&
      !strcontains(aws_iam_role_policy.runtime_rollout_orchestrator.policy, "/objects/sha256/")
    )
    error_message = "provisioner and rollout-orchestrator policies must not cross runtime-object and lineage prefixes."
  }

  assert {
    condition = (
      strcontains(aws_s3_bucket_policy.runtime_store.policy, "DenyUnconditionalRuntimeObjectWrites") &&
      strcontains(aws_s3_bucket_policy.runtime_store.policy, "DenyRuntimeObjectMutation") &&
      strcontains(aws_s3_bucket_policy.runtime_store.policy, "DenyUnconditionalRolloutWrites") &&
      strcontains(aws_s3_bucket_policy.runtime_store.policy, "DenyRolloutMutation") &&
      strcontains(aws_s3_bucket_policy.runtime_store.policy, "s3:if-none-match") &&
      strcontains(aws_s3_bucket_policy.runtime_store.policy, "s3:ObjectCreationOperation")
    )
    error_message = "bucket policy must enforce conditional creates, the multipart exception, and immutable prefixes."
  }
}
