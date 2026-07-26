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

variable "release_trust_mode" {
  description = "Compiled release attestation trust domain for Helmr binaries."
  type        = string
  default     = "production"
}

variable "release_trust_san" {
  description = "Exact GitHub Actions workflow identity compiled into development binaries."
  type        = string
  default     = null
  nullable    = true
}

variable "release_trust_source_digest" {
  description = "Exact source commit compiled into development release verification."
  type        = string
  default     = null
  nullable    = true
}

variable "release_provenance_sha256" {
  description = "SHA-256 of the locally verified development release provenance bound into the output AMI."
  type        = string
  default     = null
  nullable    = true
}

variable "release_package_s3_uri" {
  description = "Exact S3 URI for the versioned Worker runtime release package."
  type        = string
}

variable "release_package_object_arn" {
  description = "Exact S3 object ARN for release_package_s3_uri."
  type        = string
}

variable "release_package_kms_key_arn" {
  description = "Optional KMS key ARN used to encrypt the Worker runtime release package."
  type        = string
  default     = null
  nullable    = true
}

variable "release_package_version_id" {
  description = "Exact S3 object version containing the Worker runtime release package."
  type        = string
}

variable "release_package_sha256" {
  description = "Exact lowercase SHA-256 of the uncompressed Worker runtime release package tar."
  type        = string
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

variable "instance_profile_name" {
  description = "Existing EC2 instance profile for Image Builder. When null, the worker-image module creates one."
  type        = string
  default     = null
  nullable    = true
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
