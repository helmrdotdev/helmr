output "image_pipeline_arn" {
  description = "EC2 Image Builder pipeline ARN."
  value       = aws_imagebuilder_image_pipeline.worker.arn
}

output "image_recipe_arn" {
  description = "EC2 Image Builder image recipe ARN."
  value       = aws_imagebuilder_image_recipe.worker.arn
}

output "component_definition_digest" {
  description = "Canonical digest of the complete rendered Image Builder component definition."
  value       = local.component_definition_digest
}

output "image_definition_digest" {
  description = "Canonical digest of the immutable Worker image recipe inputs."
  value       = local.image_definition_digest
}

output "resolved_parent_image_id" {
  description = "Concrete parent AMI ID resolved for the current image definition."
  value       = local.parent_image
}

output "prepare_root_digest" {
  description = "Canonical digest of the root preparation helper installed by the component."
  value       = "sha256:${local.prepare_root_digest}"
}

output "root_block_device_mapping" {
  description = "Complete root block-device mapping committed by the image definition."
  value       = local.root_block_device_mapping
}

output "component_arn" {
  description = "EC2 Image Builder component ARN."
  value       = aws_imagebuilder_component.worker.arn
}

output "distribution_configuration_arn" {
  description = "EC2 Image Builder distribution configuration ARN."
  value       = aws_imagebuilder_distribution_configuration.worker.arn
}

output "distribution_regions" {
  description = "Regions configured for the current Worker image distribution."
  value       = local.distribution_regions
}

output "ami_public" {
  description = "Whether the current Worker image distribution is public."
  value       = var.ami_public
}
