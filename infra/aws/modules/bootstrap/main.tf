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
