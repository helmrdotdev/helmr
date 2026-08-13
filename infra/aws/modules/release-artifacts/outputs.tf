output "controlplane_image" {
  description = "Resolved helmr-controlplane image URI."
  value       = local.controlplane_image
}

output "worker_ami_id" {
  description = "Resolved worker AMI ID, or null when resolve_worker_ami is false and no override was supplied."
  value       = local.worker_ami_id
}

output "manifest_url" {
  description = "Release artifact manifest URL used for resolution, or null when all artifacts were overridden."
  value       = local.needs_manifest ? local.manifest_url : null
}
