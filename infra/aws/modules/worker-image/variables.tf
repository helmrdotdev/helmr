variable "name" {
  description = "Name prefix for worker image resources."
  type        = string
}

variable "source_repository_url" {
  description = "Git repository URL used to build the worker AMI."
  type        = string
  default     = "https://github.com/helmrdotdev/helmr.git"
}

variable "source_ref" {
  description = "Git ref checked out when building the worker AMI."
  type        = string
  default     = "main"
}

variable "source_bundle_s3_uri" {
  description = "Optional S3 URI for a git bundle used as the worker AMI source. When set, Image Builder clones from the bundle instead of source_repository_url."
  type        = string
  default     = null
  nullable    = true
}

variable "source_bundle_object_arn" {
  description = "Exact S3 object ARN for source_bundle_s3_uri. Required when source_bundle_s3_uri is set for least-privilege access."
  type        = string
  default     = null
  nullable    = true
}

variable "source_bundle_kms_key_arn" {
  description = "Optional KMS key ARN used to encrypt source_bundle_s3_uri."
  type        = string
  default     = null
  nullable    = true
}

variable "release_package_s3_uri" {
  description = "Exact S3 URI for the versioned Worker runtime release package."
  type        = string

  validation {
    condition     = can(regex("^s3://[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]/[A-Za-z0-9._~!$&()+,;=:@%/+\\-]+$", var.release_package_s3_uri))
    error_message = "release_package_s3_uri must be an exact S3 object URI with a shell-safe bucket and key."
  }
}

variable "release_package_object_arn" {
  description = "Exact S3 object ARN for release_package_s3_uri."
  type        = string

  validation {
    condition     = can(regex("^arn:[a-z0-9-]+:s3:::[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]/[A-Za-z0-9._~!$&()+,;=:@%/+\\-]+$", var.release_package_object_arn))
    error_message = "release_package_object_arn must be an exact S3 object ARN."
  }
}

variable "release_package_kms_key_arn" {
  description = "Optional KMS key ARN used to encrypt the Worker runtime release package."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.release_package_kms_key_arn == null || can(regex("^arn:[a-z0-9-]+:kms:[a-z0-9-]+:[0-9]{12}:key/[0-9A-Za-z/_+=,.@:-]+$", var.release_package_kms_key_arn))
    error_message = "release_package_kms_key_arn must be null or a KMS key ARN."
  }
}

variable "release_package_version_id" {
  description = "Exact S3 object version containing the Worker runtime release package."
  type        = string

  validation {
    condition     = var.release_package_version_id != "null" && length(var.release_package_version_id) <= 1024 && can(regex("^[A-Za-z0-9._~+=/-]+$", var.release_package_version_id))
    error_message = "release_package_version_id must be a non-null, shell-safe S3 version ID."
  }
}

variable "release_package_sha256" {
  description = "Exact lowercase SHA-256 of the uncompressed Worker runtime release package tar."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.release_package_sha256))
    error_message = "release_package_sha256 must be exactly 64 lowercase hexadecimal characters."
  }
}

variable "parent_image" {
  description = "Parent AMI or Image Builder image ARN. Defaults to the latest Ubuntu 24.04 amd64 server AMI."
  type        = string
  default     = null
  nullable    = true
}

variable "distribution_regions" {
  description = "AWS regions where Image Builder should distribute the worker AMI. Defaults to the provider region."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for region in var.distribution_regions : trimspace(region) != ""])
    error_message = "distribution_regions must contain non-empty region names."
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

variable "instance_profile_name" {
  description = "Existing EC2 instance profile for Image Builder. When null, this module creates a role and instance profile."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.instance_profile_name == null || trimspace(var.instance_profile_name) != ""
    error_message = "instance_profile_name must be null or a non-empty string."
  }
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
  description = "Root volume size for the AMI build instance and resulting worker AMI."
  type        = number
  default     = 120
}

variable "buildkit_slirp_cidr" {
  description = "Default IPv4 CIDR used by rootlesskit/slirp4netns in the AMI BuildKit service. Worker launch configuration can override this service at boot."
  type        = string
  default     = "198.18.0.0/24"

  validation {
    condition     = can(cidrnetmask(var.buildkit_slirp_cidr))
    error_message = "buildkit_slirp_cidr must be an IPv4 CIDR prefix."
  }
}

variable "image_version" {
  description = "Semantic version used by EC2 Image Builder resources."
  type        = string
  default     = "0.1.0"
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
