variable "helmr_version" {
  description = "Helmr release version, for example vX.Y.Z. Used to resolve official release artifacts."
  type        = string

  validation {
    condition     = trimspace(var.helmr_version) != ""
    error_message = "helmr_version must not be empty."
  }
}

variable "aws_region" {
  description = "AWS region used to select the worker AMI from the release artifact manifest."
  type        = string

  validation {
    condition     = trimspace(var.aws_region) != ""
    error_message = "aws_region must not be empty."
  }
}

variable "manifest_base_url" {
  description = "HTTPS base URL containing per-version aws-artifacts.json files."
  type        = string
  default     = "https://github.com/helmrdotdev/helmr/releases/download"
  nullable    = true

  validation {
    condition     = var.manifest_base_url == null || can(regex("^https://", var.manifest_base_url))
    error_message = "manifest_base_url must be null or an HTTPS URL."
  }
}

variable "manifest_url" {
  description = "Full HTTPS URL for the release artifact manifest. Overrides manifest_base_url when set."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.manifest_url == null || can(regex("^https://", var.manifest_url))
    error_message = "manifest_url must be null or an HTTPS URL."
  }
}

variable "control_image_override" {
  description = "Explicit control image URI for custom builds. When null, the release manifest control_image is used."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.control_image_override == null || trimspace(var.control_image_override) != ""
    error_message = "control_image_override must be null or a non-empty image URI."
  }
}

variable "worker_ami_id_override" {
  description = "Explicit worker AMI ID for custom builds. When null and resolve_worker_ami is true, the release manifest worker_amis map is used."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.worker_ami_id_override == null || trimspace(var.worker_ami_id_override) != ""
    error_message = "worker_ami_id_override must be null or a non-empty AMI ID."
  }
}

variable "resolve_worker_ami" {
  description = "Resolve a worker AMI for aws_region. Set true when worker capacity will be created."
  type        = bool
  default     = false
}
