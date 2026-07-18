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

output "runtime_store_uri" {
  description = "Immutable managed-runtime store URI ending at the objects prefix."
  value       = "s3://${aws_s3_bucket.runtime_store.bucket}/objects"
}

output "runtime_store_bucket_arn" {
  description = "S3 bucket ARN for the managed-runtime installation store."
  value       = aws_s3_bucket.runtime_store.arn
}

output "runtime_store_kms_key_arn" {
  description = "KMS key ARN for the managed-runtime installation store."
  value       = aws_kms_key.runtime_store.arn
}

output "runtime_provisioner_role_arn" {
  description = "Create-only managed-runtime provisioner role ARN."
  value       = aws_iam_role.runtime_provisioner.arn
}

output "runtime_rollout_orchestrator_role_arn" {
  description = "Append-only runtime rollout-orchestrator role ARN."
  value       = aws_iam_role.runtime_rollout_orchestrator.arn
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
