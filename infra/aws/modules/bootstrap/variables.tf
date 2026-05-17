variable "name" {
  description = "Name prefix for backend resources."
  type        = string
}

variable "bucket_name_prefix" {
  description = "Globally unique prefix for the Terraform state bucket. Defaults to name-account-region."
  type        = string
  default     = null
  nullable    = true
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}
