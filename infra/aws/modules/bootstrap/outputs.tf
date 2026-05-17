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
