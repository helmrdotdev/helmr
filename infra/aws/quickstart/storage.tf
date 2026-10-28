data "aws_caller_identity" "current" {}

resource "aws_kms_key" "helmr" {
  description             = "Helmr Quickstart CAS and secrets"
  deletion_window_in_days = 7
  enable_key_rotation     = true
}

resource "aws_s3_bucket" "cas" {
  bucket        = "${local.name}-${data.aws_caller_identity.current.account_id}-${var.aws_region}-cas"
  force_destroy = true
}

resource "aws_s3_bucket_public_access_block" "cas" {
  bucket                  = aws_s3_bucket.cas.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Object versions support the Product's storage lifecycle, not database backups.
resource "aws_s3_bucket_versioning" "cas" {
  bucket = aws_s3_bucket.cas.id
  versioning_configuration { status = "Enabled" }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "cas" {
  bucket = aws_s3_bucket.cas.id
  rule {
    apply_server_side_encryption_by_default {
      kms_master_key_id = aws_kms_key.helmr.arn
      sse_algorithm     = "aws:kms"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "cas" {
  bucket = aws_s3_bucket.cas.id
  rule {
    id     = "expire-quarantine"
    status = "Enabled"
    filter { prefix = "quarantine/" }
    expiration { days = 1 }
    noncurrent_version_expiration { noncurrent_days = 1 }
  }
  rule {
    id     = "remove-quarantine-delete-markers"
    status = "Enabled"
    filter { prefix = "quarantine/" }
    expiration { expired_object_delete_marker = true }
  }
}

resource "aws_secretsmanager_secret" "helmr" {
  for_each = toset([
    "worker_host_credential_signing_key", "auth_key", "encryption_key",
    "computer_fencing_key", "checkpoint_encryption_key", "computer_wrapping_key",
    "setup_token", "worker_enrollment_token", "github_oauth_client_secret", "capacity_token"
  ])
  name                    = "${local.name}/${each.key}"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = 0
}
