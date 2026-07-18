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

override_resource {
  target = aws_kms_key.retained_cas
  values = {
    arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000003"
  }
}

variables {
  name = "helmr-test"
  runtime_provisioner_principal_arns = [
    "arn:aws:iam::000000000000:role/runtime-provisioner"
  ]
  runtime_rollout_orchestrator_principal_arns = [
    "arn:aws:iam::000000000000:role/runtime-rollout"
  ]
}

run "retained_cas_has_a_dedicated_non_expiring_boundary" {
  command = apply

  assert {
    condition = (
      aws_s3_bucket.retained_cas.bucket != aws_s3_bucket.runtime_store.bucket &&
      aws_s3_bucket.retained_cas.bucket != aws_s3_bucket.source_artifacts.bucket &&
      aws_kms_key.retained_cas.arn != aws_kms_key.runtime_store.arn &&
      aws_kms_key.retained_cas.arn != aws_kms_key.source_artifacts.arn
    )
    error_message = "Retained deployment artifacts require a dedicated S3 and KMS boundary."
  }

  assert {
    condition     = aws_s3_bucket_versioning.retained_cas.versioning_configuration[0].status == "Enabled"
    error_message = "The retained CAS must preserve S3 object versions."
  }

  assert {
    condition = (
      output.retained_cas_uri == "s3://${aws_s3_bucket.retained_cas.bucket}" &&
      output.retained_cas_bucket_arn == aws_s3_bucket.retained_cas.arn &&
      output.retained_cas_kms_key_arn == aws_kms_key.retained_cas.arn
    )
    error_message = "Retained CAS outputs must identify the dedicated bucket and key."
  }
}

run "retained_cas_policy_is_create_only" {
  command = apply

  assert {
    condition = (
      strcontains(aws_s3_bucket_policy.retained_cas.policy, "DenyWritesOutsideCASNamespace") &&
      strcontains(aws_s3_bucket_policy.retained_cas.policy, "DenyWritesOutsideBuildWorkers") &&
      strcontains(aws_s3_bucket_policy.retained_cas.policy, "aws:PrincipalTag/helmr:RetainedCASPublisher") &&
      strcontains(aws_s3_bucket_policy.retained_cas.policy, "DenyUnconditionalObjectWrites") &&
      strcontains(aws_s3_bucket_policy.retained_cas.policy, "DenyObjectCopies") &&
      strcontains(aws_s3_bucket_policy.retained_cas.policy, "DenyObjectMutation") &&
      strcontains(aws_s3_bucket_policy.retained_cas.policy, "s3:if-none-match") &&
      strcontains(aws_s3_bucket_policy.retained_cas.policy, "s3:ObjectCreationOperation") &&
      strcontains(aws_s3_bucket_policy.retained_cas.policy, "${aws_s3_bucket.retained_cas.arn}/sha256/*") &&
      !strcontains(aws_s3_bucket_policy.retained_cas.policy, "s3:PutLifecycleConfiguration")
    )
    error_message = "The retained CAS policy must constrain writes to conditional, non-copying creates in the CAS namespace and forbid object mutation."
  }
}
