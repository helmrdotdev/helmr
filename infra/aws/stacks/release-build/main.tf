locals {
  name          = lower(var.name)
  bucket_prefix = lower("${var.name}-${data.aws_caller_identity.current.account_id}-${data.aws_region.current.region}")
}

resource "aws_kms_key" "release_artifacts" {
  description             = "KMS key for immutable Helmr release artifacts"
  deletion_window_in_days = 30
  enable_key_rotation     = true
}

resource "aws_kms_alias" "release_artifacts" {
  name          = "alias/${local.name}-release-artifacts"
  target_key_id = aws_kms_key.release_artifacts.key_id
}

resource "aws_s3_bucket" "release_artifacts" {
  bucket = "${local.bucket_prefix}-release-artifacts"
}

resource "aws_s3_bucket_versioning" "release_artifacts" {
  bucket = aws_s3_bucket.release_artifacts.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "release_artifacts" {
  bucket = aws_s3_bucket.release_artifacts.id

  rule {
    apply_server_side_encryption_by_default {
      kms_master_key_id = aws_kms_key.release_artifacts.arn
      sse_algorithm     = "aws:kms"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "release_artifacts" {
  bucket                  = aws_s3_bucket.release_artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
