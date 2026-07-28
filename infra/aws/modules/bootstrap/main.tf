locals {
  name          = lower(var.name)
  bucket_prefix = lower(coalesce(var.bucket_name_prefix, "${local.name}-${data.aws_caller_identity.current.account_id}-${data.aws_region.current.region}"))
}

resource "aws_kms_key" "terraform_state" {
  description             = "KMS key for Helmr Terraform state"
  deletion_window_in_days = 30
  enable_key_rotation     = true
  tags                    = var.tags
}

resource "aws_kms_alias" "terraform_state" {
  name          = "alias/${local.name}-terraform-state"
  target_key_id = aws_kms_key.terraform_state.key_id
}

resource "aws_s3_bucket" "terraform_state" {
  bucket = "${local.bucket_prefix}-terraform-state"
  tags   = var.tags
}

resource "aws_s3_bucket_versioning" "terraform_state" {
  bucket = aws_s3_bucket.terraform_state.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "terraform_state" {
  bucket = aws_s3_bucket.terraform_state.id

  rule {
    apply_server_side_encryption_by_default {
      kms_master_key_id = aws_kms_key.terraform_state.arn
      sse_algorithm     = "aws:kms"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "terraform_state" {
  bucket                  = aws_s3_bucket.terraform_state.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_kms_key" "source_artifacts" {
  description             = "KMS key for Helmr source artifacts"
  deletion_window_in_days = 30
  enable_key_rotation     = true
  tags                    = var.tags
}

resource "aws_kms_alias" "source_artifacts" {
  name          = "alias/${local.name}-source-artifacts"
  target_key_id = aws_kms_key.source_artifacts.key_id
}

resource "aws_s3_bucket" "source_artifacts" {
  bucket = "${local.bucket_prefix}-source-artifacts"
  tags   = var.tags
}

resource "aws_s3_bucket_versioning" "source_artifacts" {
  bucket = aws_s3_bucket.source_artifacts.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "source_artifacts" {
  bucket = aws_s3_bucket.source_artifacts.id

  rule {
    apply_server_side_encryption_by_default {
      kms_master_key_id = aws_kms_key.source_artifacts.arn
      sse_algorithm     = "aws:kms"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "source_artifacts" {
  bucket = aws_s3_bucket.source_artifacts.id

  rule {
    id     = "expire-source-bundles"
    status = "Enabled"

    filter {
      prefix = "helmr/source-bundles/"
    }

    expiration {
      days = 30
    }

    noncurrent_version_expiration {
      noncurrent_days = 7
    }
  }

  rule {
    id     = "expire-validation-evidence"
    status = "Enabled"

    filter {
      prefix = "helmr/validation-evidence/"
    }

    expiration {
      days = 30
    }

    noncurrent_version_expiration {
      noncurrent_days = 30
    }
  }
}

resource "aws_s3_bucket_public_access_block" "source_artifacts" {
  bucket                  = aws_s3_bucket.source_artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
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

resource "aws_iam_role" "platform_publisher" {
  name = "${local.name}-platform-publisher"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        AWS = var.platform_publisher_principal_arns
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "platform_publisher" {
  name = "${local.name}-platform-publisher"
  role = aws_iam_role.platform_publisher.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ListPlatformObjects"
        Effect   = "Allow"
        Action   = ["s3:ListBucket"]
        Resource = aws_s3_bucket.platform_store.arn
        Condition = {
          StringLike = {
            "s3:prefix" = [
              "objects/sha256",
              "objects/sha256/*"
            ]
          }
        }
      },
      {
        Sid    = "CreateAndVerifyPlatformObjects"
        Effect = "Allow"
        Action = [
          "s3:GetObject",
          "s3:GetObjectVersion",
          "s3:PutObject",
          "s3:AbortMultipartUpload",
          "s3:ListMultipartUploadParts"
        ]
        Resource = "${aws_s3_bucket.platform_store.arn}/objects/sha256/*"
      },
      {
        Sid    = "EncryptPlatformObjects"
        Effect = "Allow"
        Action = [
          "kms:Decrypt",
          "kms:Encrypt",
          "kms:GenerateDataKey"
        ]
        Resource = aws_kms_key.platform_store.arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = "s3.${data.aws_region.current.region}.amazonaws.com"
          }
        }
      }
    ]
  })
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

resource "aws_kms_key" "retained_cas" {
  description             = "KMS key for retained Helmr deployment artifacts"
  deletion_window_in_days = 30
  enable_key_rotation     = true
  tags                    = var.tags
}

resource "aws_kms_alias" "retained_cas" {
  name          = "alias/${local.name}-retained-cas-v0"
  target_key_id = aws_kms_key.retained_cas.key_id
}

resource "aws_s3_bucket" "retained_cas" {
  bucket        = "${local.bucket_prefix}-retained-cas-v0"
  force_destroy = true
  tags          = var.tags
}

resource "aws_s3_bucket_ownership_controls" "retained_cas" {
  bucket = aws_s3_bucket.retained_cas.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_versioning" "retained_cas" {
  bucket = aws_s3_bucket.retained_cas.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "retained_cas" {
  bucket = aws_s3_bucket.retained_cas.id

  rule {
    bucket_key_enabled = true

    apply_server_side_encryption_by_default {
      kms_master_key_id = aws_kms_key.retained_cas.arn
      sse_algorithm     = "aws:kms"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "retained_cas" {
  bucket                  = aws_s3_bucket.retained_cas.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_policy" "retained_cas" {
  bucket = aws_s3_bucket.retained_cas.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource = [
          aws_s3_bucket.retained_cas.arn,
          "${aws_s3_bucket.retained_cas.arn}/*"
        ]
        Condition = {
          Bool = {
            "aws:SecureTransport" = "false"
          }
        }
      },
      {
        Sid       = "DenyWritesOutsideCASNamespace"
        Effect    = "Deny"
        Principal = "*"
        Action = [
          "s3:PutObject",
          "s3:AbortMultipartUpload"
        ]
        NotResource = "${aws_s3_bucket.retained_cas.arn}/sha256/*"
      },
      {
        Sid       = "DenyWritesOutsideBuildWorkers"
        Effect    = "Deny"
        Principal = "*"
        Action = [
          "s3:PutObject",
          "s3:AbortMultipartUpload"
        ]
        Resource = "${aws_s3_bucket.retained_cas.arn}/sha256/*"
        Condition = {
          StringNotEquals = {
            "aws:PrincipalTag/helmr:RetainedCASPublisher" = "true"
          }
        }
      },
      {
        Sid       = "DenyUnconditionalObjectWrites"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.retained_cas.arn}/sha256/*"
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
        Sid       = "DenyObjectCopies"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.retained_cas.arn}/sha256/*"
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
        Sid       = "DenyObjectMutation"
        Effect    = "Deny"
        Principal = "*"
        Action = [
          "s3:DeleteObject",
          "s3:DeleteObjectVersion",
          "s3:PutObjectTagging",
          "s3:DeleteObjectTagging",
          "s3:PutObjectAcl",
          "s3:RestoreObject"
        ]
        Resource = "${aws_s3_bucket.retained_cas.arn}/sha256/*"
      }
    ]
  })

  depends_on = [aws_s3_bucket_public_access_block.retained_cas]
}
