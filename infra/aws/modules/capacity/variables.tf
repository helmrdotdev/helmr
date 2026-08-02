variable "name" {
  description = "Name prefix for Managed Cloud capacity automation."
  type        = string
}

variable "enabled" {
  description = "Create the scheduled one-shot capacity task. Fixed-capacity deployments leave this false."
  type        = bool
  default     = true
}

variable "vpc_id" {
  description = "VPC in which the one-shot ECS task runs."
  type        = string
}

variable "subnet_ids" {
  description = "Subnets used by the one-shot ECS task."
  type        = list(string)
}

variable "assign_public_ip" {
  description = "Whether the one-shot task receives a public IP."
  type        = bool
  default     = false
}

variable "ecs_cluster_arn" {
  description = "Existing ECS cluster ARN on which the one-shot task runs."
  type        = string
}

variable "control_url" {
  description = "External HTTPS Control URL used by the provider-neutral operator client."
  type        = string

  validation {
    condition     = !var.enabled || can(regex("^https://", var.control_url))
    error_message = "control_url must use HTTPS when capacity automation is enabled."
  }
}

variable "operator_token_secret_arn" {
  description = "Secrets Manager ARN containing the dedicated Control operator token."
  type        = string
}

variable "operator_token_kms_key_arn" {
  description = "KMS key ARN encrypting operator_token_secret_arn."
  type        = string
}

variable "control_image" {
  description = "Digest-pinned image containing helmr-aws-capacity."
  type        = string

  validation {
    condition     = !var.enabled || can(regex("@sha256:[0-9a-f]{64}$", var.control_image))
    error_message = "control_image must be digest-pinned when capacity automation is enabled."
  }
}

variable "control_image_repository_arn" {
  description = "Exact ECR repository ARN for the capacity task image."
  type        = string
}

variable "groups" {
  description = "Deployment-owned mapping from logical Worker groups to exact ASGs and host capacity policy."
  type = list(object({
    worker_group_id                 = string
    autoscaling_group_name          = string
    autoscaling_group_arn           = string
    termination_lifecycle_hook_name = string
    allows_run                      = bool
    allows_build                    = bool
    instance_capacity = object({
      cpu_millis                 = number
      memory_bytes               = number
      guest_ephemeral_disk_bytes = number
      vm_slots                   = number
      run_consumers              = number
      build_executors            = number
    })
  }))

  validation {
    condition = !var.enabled || (length(var.groups) > 0 && alltrue([
      for group in var.groups :
      trimspace(group.worker_group_id) != "" &&
      trimspace(group.autoscaling_group_name) != "" &&
      trimspace(group.termination_lifecycle_hook_name) != "" &&
      (group.allows_run || group.allows_build) &&
      group.instance_capacity.cpu_millis > 0 &&
      group.instance_capacity.memory_bytes > 0 &&
      group.instance_capacity.guest_ephemeral_disk_bytes > 0 &&
      (!group.allows_run || (
        group.instance_capacity.vm_slots > 0 &&
        group.instance_capacity.run_consumers > 0
      )) &&
      (!group.allows_build || group.instance_capacity.build_executors > 0)
    ]))
    error_message = "groups must contain complete logical-to-ASG mappings with at least one role."
  }
}

variable "schedule_expression" {
  description = "EventBridge Scheduler cadence for one-shot reconciliation."
  type        = string
  default     = "rate(1 minute)"
}

variable "observation_max_age" {
  description = "Maximum age of demand used for a new capacity decision."
  type        = string
  default     = "2m"
}

variable "reconcile_timeout" {
  description = "Hard deadline for one level-triggered reconciliation invocation."
  type        = string
  default     = "50s"
}

variable "task_cpu" {
  description = "Fargate CPU units for one reconciliation."
  type        = number
  default     = 256
}

variable "task_memory" {
  description = "Fargate memory MiB for one reconciliation."
  type        = number
  default     = 512
}

variable "log_retention_days" {
  description = "CloudWatch log retention for capacity task output."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Tags applied to capacity automation resources."
  type        = map(string)
  default     = {}
}
