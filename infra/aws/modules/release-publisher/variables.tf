variable "name" {
  description = "Name prefix for the release publisher role."
  type        = string
}

variable "principal_arns" {
  description = "AWS principal ARNs allowed to assume the create-only Platform Artifact publisher role."
  type        = list(string)

  validation {
    condition = (
      length(var.principal_arns) > 0 &&
      alltrue([
        for arn in var.principal_arns :
        can(regex("^arn:[^:]+:iam::[0-9]{12}:(role|user)/.+$", arn))
      ])
    )
    error_message = "principal_arns must contain at least one IAM role or user ARN."
  }
}

variable "platform_store_bucket_arn" {
  description = "Exact Platform Artifact bucket ARN."
  type        = string

  validation {
    condition     = can(regex("^arn:aws(-[a-z0-9-]+)?:s3:::[a-z0-9][a-z0-9.-]*$", var.platform_store_bucket_arn))
    error_message = "platform_store_bucket_arn must select one exact resource ARN, without wildcards."
  }
}

variable "platform_store_kms_key_arn" {
  description = "Exact Platform Artifact encryption key ARN."
  type        = string

  validation {
    condition     = can(regex("^arn:aws(-[a-z0-9-]+)?:kms:[a-z0-9-]+:[0-9]{12}:key/[a-zA-Z0-9-]+$", var.platform_store_kms_key_arn))
    error_message = "platform_store_kms_key_arn must select one exact resource ARN, without wildcards."
  }
}

variable "controlplane_release_repository_arn" {
  description = "Exact Control Plane release repository ARN."
  type        = string

  validation {
    condition     = can(regex("^arn:aws(-[a-z0-9-]+)?:ecr:[a-z0-9-]+:[0-9]{12}:repository/[a-z0-9][a-z0-9._/-]*$", var.controlplane_release_repository_arn))
    error_message = "controlplane_release_repository_arn must select one exact resource ARN, without wildcards."
  }
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
