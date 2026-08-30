output "release_artifact_bucket_name" {
  description = "S3 bucket name for immutable release artifacts consumed by image infrastructure."
  value       = aws_s3_bucket.release_artifacts.bucket
}

output "release_artifact_bucket_arn" {
  description = "S3 bucket ARN for immutable release artifacts consumed by image infrastructure."
  value       = aws_s3_bucket.release_artifacts.arn
}

output "release_artifact_kms_key_arn" {
  description = "KMS key ARN for immutable release artifact encryption."
  value       = aws_kms_key.release_artifacts.arn
}

output "controlplane_release_repository_url" {
  description = "Durable ECR repository URL for trusted Control Plane release images."
  value       = aws_ecr_repository.controlplane_releases.repository_url
}

output "controlplane_release_repository_name" {
  description = "Durable ECR repository name for trusted Control Plane release images."
  value       = aws_ecr_repository.controlplane_releases.name
}

output "controlplane_release_repository_arn" {
  description = "Durable ECR repository ARN for trusted Control Plane release images."
  value       = aws_ecr_repository.controlplane_releases.arn
}

output "controlplane_release_kms_key_arn" {
  description = "KMS key ARN for trusted Control Plane release images."
  value       = aws_kms_key.controlplane_releases.arn
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
