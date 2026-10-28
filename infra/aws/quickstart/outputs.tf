output "worker_autoscaling_group_name" {
  description = "Current execution Worker Auto Scaling group name when create_worker is true."
  value       = try(module.worker_group[local.worker_pool_name].autoscaling_group_name, null)
}

output "worker_autoscaling_group_arn" {
  description = "Exact current execution Worker Auto Scaling group ARN."
  value       = try(module.worker_group[local.worker_pool_name].autoscaling_group_arn, null)
}

output "worker_protect_from_scale_in" {
  description = "Whether new execution Worker instances start protected from scale in."
  value       = try(module.worker_group[local.worker_pool_name].protect_from_scale_in, null)
}

output "worker_iam_role_name" {
  description = "Execution Worker IAM role name when create_worker is true."
  value       = try(module.worker_group[local.worker_pool_name].iam_role_name, null)
}

output "worker_generation_definitions" {
  description = "Complete immutable generation inputs keyed by Product Pool name. Persist an old entry in retained_worker_generations until its restore authority is explicitly retired."
  value = {
    for pool_name, generation in local.worker_generations :
    pool_name => {
      generation_inputs = generation.generation_inputs
      count             = generation.count
      sealed_provider_definition = try(
        module.worker_group[pool_name].sealed_provider_definition,
        generation.sealed_provider_definition,
      )
    }
  }
}

output "worker_generation_bindings" {
  description = "Exact Product Pool to provider ASG/launch-template bindings for all current and retained generations."
  value = {
    for pool_name, generation in local.worker_generations :
    pool_name => {
      provider_name           = generation.provider_name
      autoscaling_group_name  = try(module.worker_group[pool_name].autoscaling_group_name, null)
      autoscaling_group_arn   = try(module.worker_group[pool_name].autoscaling_group_arn, null)
      launch_template_id      = try(module.worker_group[pool_name].launch_template_id, null)
      launch_template_version = try(module.worker_group[pool_name].launch_template_version, null)
      ami_id                  = generation.ami_id
      count                   = generation.count
    }
  }
}


output "controlplane_url" {
  value = var.app_origin
}

output "controlplane_public_ip" {
  description = "Point the application's DNS A record directly at this address."
  value       = aws_eip.control.public_ip
}

output "control_instance_id" {
  description = "Control host for SSM Session Manager."
  value       = aws_instance.control.id
}

output "controlplane_image" {
  value = module.release_artifacts.controlplane_image
}

output "worker_ami_id" {
  value = module.release_artifacts.worker_ami_id
}

output "worker_controlplane_url" {
  value = var.app_origin
}

output "secret_arns" {
  description = "Populate with the existing secret bootstrap helper before starting Helmr."
  value       = { for key, secret in aws_secretsmanager_secret.helmr : key => secret.arn }
}

output "worker_enrollment_secret_arn" {
  value = aws_secretsmanager_secret.helmr["worker_enrollment_token"].arn
}

output "capacity_token_secret_arn" {
  value = aws_secretsmanager_secret.helmr["capacity_token"].arn
}

output "cas_uri" {
  value = "s3://${aws_s3_bucket.cas.bucket}"
}
