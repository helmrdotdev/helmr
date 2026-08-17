output "image_pipeline_arn" {
  description = "EC2 Image Builder pipeline ARN."
  value       = module.worker_image.image_pipeline_arn
}

output "image_recipe_arn" {
  description = "EC2 Image Builder image recipe ARN."
  value       = module.worker_image.image_recipe_arn
}


output "component_arn" {
  description = "EC2 Image Builder component ARN."
  value       = module.worker_image.component_arn
}

output "component_definition_digest" {
  description = "Canonical digest of the complete rendered Image Builder component definition."
  value       = module.worker_image.component_definition_digest
}

output "image_definition_digest" {
  description = "Canonical digest of the immutable Worker image recipe inputs."
  value       = module.worker_image.image_definition_digest
}

output "resolved_parent_image_id" {
  description = "Concrete parent AMI ID resolved for the current image definition."
  value       = module.worker_image.resolved_parent_image_id
}

output "prepare_root_digest" {
  description = "Canonical digest of the root preparation helper installed by the component."
  value       = module.worker_image.prepare_root_digest
}

output "root_block_device_mapping" {
  description = "Complete root block-device mapping committed by the image definition."
  value       = module.worker_image.root_block_device_mapping
}

output "distribution_configuration_arn" {
  description = "EC2 Image Builder distribution configuration ARN."
  value       = module.worker_image.distribution_configuration_arn
}

output "distribution_regions" {
  description = "Regions configured for Worker image distribution."
  value       = module.worker_image.distribution_regions
}

output "ami_public" {
  description = "Whether distributed Worker AMIs are public."
  value       = module.worker_image.ami_public
}
