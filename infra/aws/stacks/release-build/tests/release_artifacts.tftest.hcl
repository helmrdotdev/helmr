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

variables {
  name = "helmr-test"
}

run "release_artifacts_are_durable_encrypted_and_private" {
  command = apply

  assert {
    condition = (
      aws_s3_bucket.release_artifacts.bucket == "helmr-test-000000000000-us-east-1-release-artifacts" &&
      !coalesce(aws_s3_bucket.release_artifacts.force_destroy, false) &&
      aws_s3_bucket_versioning.release_artifacts.versioning_configuration[0].status == "Enabled" &&
      one(one(aws_s3_bucket_server_side_encryption_configuration.release_artifacts.rule).apply_server_side_encryption_by_default).sse_algorithm == "aws:kms" &&
      one(one(aws_s3_bucket_server_side_encryption_configuration.release_artifacts.rule).apply_server_side_encryption_by_default).kms_master_key_id == aws_kms_key.release_artifacts.arn &&
      aws_s3_bucket_public_access_block.release_artifacts.block_public_acls &&
      aws_s3_bucket_public_access_block.release_artifacts.block_public_policy &&
      aws_s3_bucket_public_access_block.release_artifacts.ignore_public_acls &&
      aws_s3_bucket_public_access_block.release_artifacts.restrict_public_buckets
    )
    error_message = "release artifacts must use one durable, versioned, KMS-encrypted, non-public bucket."
  }

  assert {
    condition     = output.release_artifact_bucket_name == aws_s3_bucket.release_artifacts.bucket
    error_message = "release-build must expose the artifact bucket."
  }
}
