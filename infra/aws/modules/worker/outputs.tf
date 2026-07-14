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

output "protect_from_scale_in" {
  description = "Whether new worker instances start protected from scale in."
  value       = aws_autoscaling_group.worker.protect_from_scale_in
}
