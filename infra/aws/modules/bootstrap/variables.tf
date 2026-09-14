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

variable "create_platform_publisher" {
  description = "Create the Platform Artifact publisher role and its inline policy."
  type        = bool
  default     = true
  nullable    = false
}

variable "platform_publisher_principal_arns" {
  description = "AWS principal ARNs allowed to assume the create-only Platform Artifact publisher role."
  type        = list(string)
  default     = []

  validation {
    condition = !var.create_platform_publisher || (
      length(var.platform_publisher_principal_arns) > 0 &&
      alltrue([
        for arn in var.platform_publisher_principal_arns :
        can(regex("^arn:[^:]+:iam::[0-9]{12}:(role|user)/.+$", arn))
      ])
    )
    error_message = "platform_publisher_principal_arns must contain at least one valid IAM role or user ARN when create_platform_publisher is true."
  }
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
