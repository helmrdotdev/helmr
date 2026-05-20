variable "aws_region" {
  description = "AWS region."
  type        = string
}

variable "name" {
  description = "Deployment name."
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
  description = "Optional S3 URI for a git bundle used as the worker AMI source."
  type        = string
  default     = null
  nullable    = true
}

variable "source_bundle_object_arn" {
  description = "Exact S3 object ARN for source_bundle_s3_uri."
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

variable "parent_image" {
  description = "Optional parent AMI or Image Builder image ARN."
  type        = string
  default     = null
  nullable    = true
}

variable "distribution_regions" {
  description = "AWS regions where Image Builder should distribute the worker AMI. Defaults to the provider region."
  type        = list(string)
  default     = []
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

variable "image_version" {
  description = "Semantic version used by EC2 Image Builder resources."
  type        = string
  default     = "0.1.0"
}

variable "buildkit_slirp_cidr" {
  description = "Default IPv4 CIDR used by rootlesskit/slirp4netns in the AMI BuildKit service."
  type        = string
  default     = "198.18.0.0/24"
}
