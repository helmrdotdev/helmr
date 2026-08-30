output "release_artifact_bucket_name" {
  description = "S3 bucket name for immutable release artifacts consumed by image infrastructure."
  value       = aws_s3_bucket.release_artifacts.bucket
}
