locals {
  name          = lower(var.name)
  bucket_prefix = lower(coalesce(var.bucket_name_prefix, "${local.name}-${data.aws_caller_identity.current.account_id}-${data.aws_region.current.region}"))
}

resource "aws_kms_key" "release_artifacts" {
  description             = "KMS key for immutable Helmr release artifacts"
  deletion_window_in_days = 30
  enable_key_rotation     = true
  tags                    = var.tags
}

resource "aws_kms_alias" "release_artifacts" {
  name          = "alias/${local.name}-release-artifacts"
  target_key_id = aws_kms_key.release_artifacts.key_id
}

resource "aws_s3_bucket" "release_artifacts" {
  bucket = "${local.bucket_prefix}-release-artifacts"
  tags   = var.tags
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

resource "aws_kms_key" "controlplane_releases" {
  description             = "KMS key for trusted Helmr Control Plane release images"
  deletion_window_in_days = 30
  enable_key_rotation     = true
  tags                    = var.tags
}

resource "aws_kms_alias" "controlplane_releases" {
  name          = "alias/${local.name}-controlplane-releases"
  target_key_id = aws_kms_key.controlplane_releases.key_id
}

resource "aws_ecr_repository" "controlplane_releases" {
  name                 = "${local.name}/controlplane-releases"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = false
  tags                 = var.tags

  encryption_configuration {
    encryption_type = "KMS"
    kms_key         = aws_kms_key.controlplane_releases.arn
  }

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_kms_key" "platform_store" {
  description             = "KMS key for the Helmr immutable release store"
  deletion_window_in_days = 30
  enable_key_rotation     = true
  tags                    = var.tags
}

resource "aws_kms_alias" "platform_store" {
  name          = "alias/${local.name}-platform-store"
  target_key_id = aws_kms_key.platform_store.key_id
}

resource "aws_s3_bucket" "platform_store" {
  bucket        = "${local.bucket_prefix}-platform-store"
  force_destroy = true
  tags          = var.tags
}

resource "aws_s3_bucket_ownership_controls" "platform_store" {
  bucket = aws_s3_bucket.platform_store.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_versioning" "platform_store" {
  bucket = aws_s3_bucket.platform_store.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "platform_store" {
  bucket = aws_s3_bucket.platform_store.id

  rule {
    bucket_key_enabled = true

    apply_server_side_encryption_by_default {
      kms_master_key_id = aws_kms_key.platform_store.arn
      sse_algorithm     = "aws:kms"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "platform_store" {
  bucket                  = aws_s3_bucket.platform_store.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_policy" "platform_store" {
  bucket = aws_s3_bucket.platform_store.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
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
    ]
  })

  depends_on = [aws_s3_bucket_public_access_block.platform_store]
}
