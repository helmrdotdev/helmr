output "security_group_id" {
  description = "Worker security group ID."
  value       = aws_security_group.worker.id
}

output "instance_profile_name" {
  description = "Worker instance profile name."
  value       = aws_iam_instance_profile.worker.name
}

output "iam_role_name" {
  description = "Worker IAM role name."
  value       = aws_iam_role.worker.name
}

output "autoscaling_group_name" {
  description = "Worker Auto Scaling group name."
  value       = aws_autoscaling_group.worker.name
}

output "autoscaling_group_arn" {
  description = "Worker Auto Scaling group ARN."
  value       = aws_autoscaling_group.worker.arn
}

output "launch_template_id" {
  description = "Launch template ID used by the Worker Auto Scaling group."
  value       = aws_launch_template.worker.id
}

output "launch_template_version" {
  description = "Exact launch template version used by the Worker Auto Scaling group."
  value       = tostring(aws_launch_template.worker.latest_version)
}

output "protect_from_scale_in" {
  description = "Whether new worker instances start protected from scale in."
  value       = aws_autoscaling_group.worker.protect_from_scale_in
}

output "termination_lifecycle_hook_name" {
  description = "Exact termination hook heartbeated and completed by the terminating Worker host."
  value       = local.termination_hook_name
}
