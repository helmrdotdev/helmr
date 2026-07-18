variable "name" {
  description = "Name prefix for backend resources."
  type        = string
}

variable "bucket_name_prefix" {
  description = "Globally unique prefix for bootstrap buckets. Defaults to name-account-region."
  type        = string
  default     = null
  nullable    = true
}

variable "runtime_provisioner_principal_arns" {
  description = "AWS principal ARNs allowed to assume the create-only managed-runtime provisioner role."
  type        = list(string)

  validation {
    condition = (
      length(var.runtime_provisioner_principal_arns) > 0 &&
      alltrue([
        for arn in var.runtime_provisioner_principal_arns :
        can(regex("^arn:[^:]+:iam::[0-9]{12}:(role|user)/.+$", arn))
      ])
    )
    error_message = "runtime_provisioner_principal_arns must contain at least one IAM role or user ARN."
  }
}

variable "runtime_rollout_orchestrator_principal_arns" {
  description = "AWS principal ARNs allowed to assume the append-only runtime rollout-orchestrator role."
  type        = list(string)

  validation {
    condition = (
      length(var.runtime_rollout_orchestrator_principal_arns) > 0 &&
      alltrue([
        for arn in var.runtime_rollout_orchestrator_principal_arns :
        can(regex("^arn:[^:]+:iam::[0-9]{12}:(role|user)/.+$", arn))
      ])
    )
    error_message = "runtime_rollout_orchestrator_principal_arns must contain at least one IAM role or user ARN."
  }
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
