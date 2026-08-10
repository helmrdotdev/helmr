variable "name" {
  description = "Name prefix for worker image resources."
  type        = string

  validation {
    condition = (
      length(var.name) <= 43 &&
      can(regex("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$", var.name))
    )
    error_message = "name must be a lowercase AWS name prefix no longer than 43 characters so every derived resource name, including full definition digests, fits its service limit."
  }
}

variable "host_artifacts_manifest_digest" {
  description = "Canonical SHA-256 digest of worker-host-artifacts.json."
  type        = string

  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.host_artifacts_manifest_digest))
    error_message = "host_artifacts_manifest_digest must be a canonical SHA-256 digest."
  }
}

variable "host_artifacts_bundle_s3_uri" {
  description = "S3 URI of the exact Worker host artifact bundle."
  type        = string

  validation {
    condition     = can(regex("^s3://[^/]+/.+$", var.host_artifacts_bundle_s3_uri))
    error_message = "host_artifacts_bundle_s3_uri must be an S3 object URI."
  }
}

variable "host_artifacts_bundle_object_arn" {
  description = "Exact S3 object ARN for host_artifacts_bundle_s3_uri."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:s3:::[^/]+/.+$", var.host_artifacts_bundle_object_arn))
    error_message = "host_artifacts_bundle_object_arn must be an S3 object ARN."
  }
}

variable "host_artifacts_bundle_digest" {
  description = "Canonical SHA-256 digest of the exact Worker host artifact bundle."
  type        = string

  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.host_artifacts_bundle_digest))
    error_message = "host_artifacts_bundle_digest must be a canonical SHA-256 digest."
  }
}

variable "host_artifacts_bundle_kms_key_arn" {
  description = "Optional KMS key ARN used to encrypt host_artifacts_bundle_s3_uri."
  type        = string
  default     = null
  nullable    = true
}

variable "runtime_artifacts_manifest_digest" {
  description = "Canonical SHA-256 digest of runtime-artifacts.json that the image build must install exactly."
  type        = string

  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.runtime_artifacts_manifest_digest))
    error_message = "runtime_artifacts_manifest_digest must be a canonical SHA-256 digest."
  }
}

variable "runtime_artifacts_bundle_s3_uri" {
  description = "S3 URI of the exact runtime artifact bundle installed into the worker AMI."
  type        = string

  validation {
    condition     = can(regex("^s3://[^/]+/.+$", var.runtime_artifacts_bundle_s3_uri))
    error_message = "runtime_artifacts_bundle_s3_uri must be an S3 object URI."
  }
}

variable "runtime_artifacts_bundle_object_arn" {
  description = "Exact S3 object ARN for runtime_artifacts_bundle_s3_uri."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:s3:::[^/]+/.+$", var.runtime_artifacts_bundle_object_arn))
    error_message = "runtime_artifacts_bundle_object_arn must be an S3 object ARN."
  }
}

variable "runtime_artifacts_bundle_digest" {
  description = "Canonical SHA-256 digest of the exact runtime artifact bundle."
  type        = string

  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.runtime_artifacts_bundle_digest))
    error_message = "runtime_artifacts_bundle_digest must be a canonical SHA-256 digest."
  }
}

variable "runtime_artifacts_bundle_kms_key_arn" {
  description = "Optional KMS key ARN used to encrypt runtime_artifacts_bundle_s3_uri."
  type        = string
  default     = null
  nullable    = true
}

variable "parent_image" {
  description = "Concrete parent AMI ID. Defaults to the latest Ubuntu 24.04 amd64 server AMI resolved during apply."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.parent_image == null || can(regex("^ami-([0-9a-f]{8}|[0-9a-f]{17})$", var.parent_image))
    error_message = "parent_image must be a concrete AMI ID."
  }
}

variable "distribution_regions" {
  description = "AWS regions where Image Builder should distribute the worker AMI. Defaults to the provider region."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for region in var.distribution_regions : can(regex("^[a-z]{2}-[a-z-]+-[0-9]+$", region))])
    error_message = "distribution_regions must contain AWS Region names."
  }
}

variable "ami_public" {
  description = "Make distributed worker AMIs public. Public AMIs must not contain encrypted snapshots."
  type        = bool
  default     = false
}

variable "root_volume_encrypted" {
  description = "Encrypt the worker AMI root volume snapshot. Set false for public official AMIs."
  type        = bool
  default     = true
}

variable "instance_types" {
  description = "Instance types Image Builder may use for AMI builds."
  type        = list(string)
  default     = ["c8i.xlarge"]
}

variable "subnet_id" {
  description = "Optional subnet for Image Builder build instances."
  type        = string
  default     = null
  nullable    = true
}

variable "security_group_ids" {
  description = "Optional security groups for Image Builder build instances."
  type        = list(string)
  default     = []
}

variable "root_volume_size_gb" {
  description = "Root volume size for the install-only AMI build and resulting snapshot. Worker launch templates independently choose their runtime volume size."
  type        = number
  default     = 24
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
