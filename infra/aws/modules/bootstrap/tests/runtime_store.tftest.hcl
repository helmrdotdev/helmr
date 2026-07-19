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
  target = aws_kms_key.manager_store
  values = {
    arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000003"
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

run "manager_store_is_dedicated_versioned_and_create_only" {
  command = apply

  assert {
    condition = (
      aws_s3_bucket.manager_store.bucket != aws_s3_bucket.runtime_store.bucket &&
      aws_s3_bucket.manager_store.bucket != aws_s3_bucket.source_artifacts.bucket &&
      aws_kms_key.manager_store.arn != aws_kms_key.runtime_store.arn &&
      aws_kms_key.manager_store.arn != aws_kms_key.source_artifacts.arn
    )
    error_message = "package-manager authority requires a dedicated S3 and KMS boundary."
  }

  assert {
    condition     = aws_s3_bucket_versioning.manager_store.versioning_configuration[0].status == "Enabled"
    error_message = "package-manager authority store must have versioning enabled."
  }

  assert {
    condition = (
      output.manager_store_uri == "s3://${aws_s3_bucket.manager_store.bucket}" &&
      output.manager_store_bucket_arn == aws_s3_bucket.manager_store.arn &&
      output.manager_store_kms_key_arn == aws_kms_key.manager_store.arn
    )
    error_message = "bootstrap outputs must expose the package-manager store URI and physical authority."
  }

  assert {
    condition = (
      strcontains(aws_s3_bucket_policy.manager_store.policy, jsonencode({
        Sid       = "DenyUnconditionalManagerWrites"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.manager_store.arn}/v0/*"
        Condition = {
          Null = {
            "s3:if-none-match" = "true"
          }
          Bool = {
            "s3:ObjectCreationOperation" = "false"
          }
        }
      })) &&
      strcontains(aws_s3_bucket_policy.manager_store.policy, jsonencode({
        Sid       = "DenyManagerCopies"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.manager_store.arn}/v0/*"
        Condition = {
          Null = {
            "s3:x-amz-copy-source" = "false"
          }
          Bool = {
            "s3:ObjectCreationOperation" = "false"
          }
        }
      })) &&
      strcontains(aws_s3_bucket_policy.manager_store.policy, jsonencode({
        Sid       = "DenyExplicitNonKMSManagerEncryption"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.manager_store.arn}/v0/*"
        Condition = {
          Null = {
            "s3:x-amz-server-side-encryption" = "false"
          }
          StringNotEquals = {
            "s3:x-amz-server-side-encryption" = "aws:kms"
          }
        }
      })) &&
      strcontains(aws_s3_bucket_policy.manager_store.policy, jsonencode({
        Sid       = "DenyExplicitWrongManagerKMSKey"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.manager_store.arn}/v0/*"
        Condition = {
          Null = {
            "s3:x-amz-server-side-encryption-aws-kms-key-id" = "false"
          }
          StringNotEquals = {
            "s3:x-amz-server-side-encryption-aws-kms-key-id" = aws_kms_key.manager_store.arn
          }
        }
      })) &&
      strcontains(jsonencode(aws_s3_bucket_server_side_encryption_configuration.manager_store.rule), "\"sse_algorithm\":\"aws:kms\"") &&
      strcontains(jsonencode(aws_s3_bucket_server_side_encryption_configuration.manager_store.rule), "\"kms_master_key_id\":\"${aws_kms_key.manager_store.arn}\"")
    )
    error_message = "package-manager storage must enforce create-only writes and the dedicated default KMS key."
  }

  assert {
    condition = (
      strcontains(aws_s3_bucket_policy.manager_store.policy, "DenyManagerWritesOutsideNamespace") &&
      strcontains(aws_s3_bucket_policy.manager_store.policy, "DenyManagerMutation")
    )
    error_message = "package-manager bucket policy must reject writes outside its namespace and every mutation API."
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
