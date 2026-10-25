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
  target = aws_kms_key.release_artifacts
  values = {
    arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000001"
  }
}

override_resource {
  target = aws_kms_key.platform_store
  values = {
    arn = "arn:aws:kms:us-east-1:000000000000:key/00000000-0000-0000-0000-000000000002"
  }
}

variables {
  name = "helmr-test"
}

run "platform_store_is_versioned_non_expiring_and_separate" {
  command = apply

  assert {
    condition     = aws_s3_bucket.platform_store.bucket != aws_s3_bucket.release_artifacts.bucket && aws_kms_key.platform_store.arn != aws_kms_key.release_artifacts.arn
    error_message = "managed runtime must use a dedicated bucket and KMS key."
  }

  assert {
    condition     = aws_s3_bucket_versioning.platform_store.versioning_configuration[0].status == "Enabled"
    error_message = "Platform Artifact store must have versioning enabled."
  }

  assert {
    condition = (
      aws_s3_bucket_versioning.release_artifacts.versioning_configuration[0].status == "Enabled" &&
      !coalesce(aws_s3_bucket.release_artifacts.force_destroy, false) &&
      output.release_artifact_bucket_name == aws_s3_bucket.release_artifacts.bucket &&
      output.release_artifact_bucket_arn == aws_s3_bucket.release_artifacts.arn &&
      output.release_artifact_kms_key_arn == aws_kms_key.release_artifacts.arn
    )
    error_message = "release artifacts must be durable, versioned, and exposed through exact release storage outputs."
  }

  assert {
    condition     = aws_s3_bucket.platform_store.force_destroy
    error_message = "explicit whole-installation teardown must be able to empty all versions and delete markers after policy guards are removed."
  }

  assert {
    condition = (
      output.platform_store_uri == "s3://${aws_s3_bucket.platform_store.bucket}/objects" &&
      output.platform_store_bucket_arn == aws_s3_bucket.platform_store.arn &&
      output.platform_store_kms_key_arn == aws_kms_key.platform_store.arn
    )
    error_message = "release storage outputs must expose the fixed /objects URI and its physical bucket/KMS authority."
  }
}

run "platform_store_immutability_is_bounded" {
  command = apply


  assert {
    condition = (
      toset([for statement in jsondecode(aws_s3_bucket_policy.platform_store.policy).Statement : jsonencode(merge(statement, { Action = sort(flatten([statement.Action])), Resource = sort(flatten([statement.Resource])) }))]) ==
      toset([for statement in [
        {
          Sid       = "DenyInsecureTransport"
          Effect    = "Deny"
          Principal = "*"
          Action    = "s3:*"
          Resource = [
            aws_s3_bucket.platform_store.arn,
            "${aws_s3_bucket.platform_store.arn}/*"
          ]
          Condition = {
            Bool = {
              "aws:SecureTransport" = "false"
            }
          }
        },
        {
          Sid       = "DenyUnconditionalPlatformObjectWrites"
          Effect    = "Deny"
          Principal = "*"
          Action    = "s3:PutObject"
          Resource  = "${aws_s3_bucket.platform_store.arn}/objects/sha256/*"
          Condition = {
            Null = {
              "s3:if-none-match" = "true"
            }
            Bool = {
              "s3:ObjectCreationOperation" = "true"
            }
          }
        },
        {
          Sid       = "DenyPlatformObjectCopies"
          Effect    = "Deny"
          Principal = "*"
          Action    = "s3:PutObject"
          Resource  = "${aws_s3_bucket.platform_store.arn}/objects/sha256/*"
          Condition = {
            Null = {
              "s3:x-amz-copy-source" = "false"
            }
            Bool = {
              "s3:ObjectCreationOperation" = "true"
            }
          }
        },
        {
          Sid       = "DenyPlatformObjectMutation"
          Effect    = "Deny"
          Principal = "*"
          Action = [
            "s3:DeleteObject",
            "s3:DeleteObjectVersion",
            "s3:PutObjectTagging",
            "s3:DeleteObjectTagging"
          ]
          Resource = "${aws_s3_bucket.platform_store.arn}/objects/sha256/*"
        }
      ] : jsonencode(merge(statement, { Action = sort(flatten([statement.Action])), Resource = sort(flatten([statement.Resource])) }))])
    )
    error_message = "bucket policy must enforce conditional creates, the multipart exception, and immutable objects."
  }
}
