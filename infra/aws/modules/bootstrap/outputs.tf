output "bucket_name" {
  description = "S3 bucket name for Terraform state."
  value       = aws_s3_bucket.terraform_state.bucket
}

output "kms_key_arn" {
  description = "KMS key ARN for Terraform state encryption."
  value       = aws_kms_key.terraform_state.arn
}

output "source_artifact_bucket_name" {
  description = "S3 bucket name for source artifacts consumed by build infrastructure."
  value       = aws_s3_bucket.source_artifacts.bucket
}

output "source_artifact_bucket_arn" {
  description = "S3 bucket ARN for source artifacts consumed by build infrastructure."
  value       = aws_s3_bucket.source_artifacts.arn
}

output "source_artifact_kms_key_arn" {
  description = "KMS key ARN for source artifact encryption."
  value       = aws_kms_key.source_artifacts.arn
}

output "platform_store_uri" {
  description = "Immutable Helmr release store URI ending at the objects prefix."
  value       = "s3://${aws_s3_bucket.platform_store.bucket}/objects"
}

output "platform_store_bucket_arn" {
  description = "S3 bucket ARN for the immutable Helmr release store."
  value       = aws_s3_bucket.platform_store.arn
}

output "platform_store_kms_key_arn" {
  description = "KMS key ARN for the immutable Helmr release store."
  value       = aws_kms_key.platform_store.arn
}

output "platform_publisher_role_arn" {
  description = "Create-only Platform Artifact publisher role ARN."
  value       = aws_iam_role.platform_publisher.arn
}

output "retained_cas_uri" {
  description = "Retained deployment Artifact CAS URI."
  value       = "s3://${aws_s3_bucket.retained_cas.bucket}"
}

output "retained_cas_bucket_arn" {
  description = "S3 bucket ARN for retained deployment artifacts."
  value       = aws_s3_bucket.retained_cas.arn
}

output "retained_cas_kms_key_arn" {
  description = "KMS key ARN for retained deployment artifacts."
  value       = aws_kms_key.retained_cas.arn
}
