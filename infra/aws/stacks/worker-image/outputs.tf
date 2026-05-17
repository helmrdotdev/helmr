output "image_pipeline_arn" {
  description = "EC2 Image Builder pipeline ARN."
  value       = module.worker_image.image_pipeline_arn
}

output "image_recipe_arn" {
  description = "EC2 Image Builder image recipe ARN."
  value       = module.worker_image.image_recipe_arn
}

output "instance_profile_name" {
  description = "Image Builder instance profile name."
  value       = module.worker_image.instance_profile_name
}
