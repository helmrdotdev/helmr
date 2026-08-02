output "task_definition_arn" {
  description = "One-shot capacity task definition ARN when enabled."
  value       = try(aws_ecs_task_definition.capacity[0].arn, null)
}

output "schedule_arn" {
  description = "EventBridge Scheduler ARN when enabled."
  value       = try(aws_scheduler_schedule.capacity[0].arn, null)
}

output "task_role_arn" {
  description = "AWS-only task role used by capacity automation."
  value       = try(aws_iam_role.task[0].arn, null)
}
