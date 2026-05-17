output "control_image" {
  description = "Resolved helmr-control image URI."
  value       = terraform_data.resolved.output.control_image
}

output "worker_ami_id" {
  description = "Resolved worker AMI ID, or null when resolve_worker_ami is false and no override was supplied."
  value       = terraform_data.resolved.output.worker_ami_id
}

output "manifest_url" {
  description = "Release artifact manifest URL used for resolution, or null when all artifacts were overridden."
  value       = terraform_data.resolved.output.manifest_url
}
