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
  value       = var.sealed_provider_definition == null ? tostring(aws_launch_template.worker.latest_version) : var.sealed_provider_definition.launch_template_version
}

output "sealed_provider_definition" {
  description = "Exact realized user-data/IAM/launch authority to retain with this immutable Worker Pool generation."
  value = {
    user_data_base64                                = local.worker_user_data_base64
    permission_policy_json                          = local.worker_permission_policy_json
    boundary_policy_json                            = local.worker_boundary_policy_json
    enable_ssm                                      = local.worker_enable_ssm
    launch_template_version                         = var.sealed_provider_definition == null ? tostring(aws_launch_template.worker.latest_version) : var.sealed_provider_definition.launch_template_version
    health_check_grace_period_seconds               = local.worker_health_check_grace_period_seconds
    launch_lifecycle_heartbeat_timeout_seconds      = local.worker_launch_lifecycle_heartbeat_timeout_seconds
    termination_lifecycle_heartbeat_timeout_seconds = local.worker_termination_lifecycle_heartbeat_timeout_seconds
    termination_drain_timeout_seconds               = local.worker_termination_drain_timeout_seconds
    lifecycle_heartbeat_interval_seconds            = local.worker_lifecycle_heartbeat_interval_seconds
    termination_policies                            = local.worker_termination_policies
    protect_from_scale_in                           = local.worker_protect_from_scale_in
    health_check_type                               = local.worker_health_check_type
    instance_refresh_strategy                       = local.worker_instance_refresh_strategy
    instance_refresh_min_healthy_percentage         = local.worker_instance_refresh_min_healthy_percentage
    instance_refresh_max_healthy_percentage         = local.worker_instance_refresh_max_healthy_percentage
    instance_refresh_scale_in_protected_instances   = local.worker_instance_refresh_scale_in_protected_instances
    instance_refresh_standby_instances              = local.worker_instance_refresh_standby_instances
    instance_refresh_skip_matching                  = local.worker_instance_refresh_skip_matching
    launch_lifecycle_transition                     = local.worker_launch_lifecycle_transition
    launch_lifecycle_default_result                 = local.worker_launch_lifecycle_default_result
    termination_lifecycle_transition                = local.worker_termination_lifecycle_transition
    termination_lifecycle_default_result            = local.worker_termination_lifecycle_default_result
  }
}

output "launch_lifecycle_hook_name" {
  description = "Exact launch hook completed after Worker readiness."
  value       = local.launch_hook_name
}

output "protect_from_scale_in" {
  description = "Whether new worker instances start protected from scale in."
  value       = aws_autoscaling_group.worker.protect_from_scale_in
}

output "termination_lifecycle_hook_name" {
  description = "Exact termination hook heartbeated and completed by the terminating Worker host."
  value       = local.termination_hook_name
}
