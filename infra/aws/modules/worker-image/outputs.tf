output "image_pipeline_arn" {
  description = "EC2 Image Builder pipeline ARN."
  value       = aws_imagebuilder_image_pipeline.worker.arn
}

output "image_recipe_arn" {
  description = "EC2 Image Builder image recipe ARN."
  value       = aws_imagebuilder_image_recipe.worker.arn
}

output "component_arn" {
  description = "EC2 Image Builder component ARN."
  value       = aws_imagebuilder_component.worker.arn
}

output "distribution_configuration_arn" {
  description = "EC2 Image Builder distribution configuration ARN."
  value       = aws_imagebuilder_distribution_configuration.worker.arn
}

output "instance_profile_name" {
  description = "Image Builder instance profile name."
  value       = local.instance_profile_name
}
