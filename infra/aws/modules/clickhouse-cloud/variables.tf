variable "name" {
  description = "Name prefix for ClickHouse Cloud and AWS PrivateLink resources."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,32}[a-z0-9]$", var.name))
    error_message = "name must be 3-34 characters, start with a lowercase letter, end with a lowercase letter or number, and contain only lowercase letters, numbers, and hyphens."
  }
}

variable "service_name" {
  description = "ClickHouse Cloud service name."
  type        = string
  default     = null
  nullable    = true
}

variable "clickhouse_region" {
  description = "ClickHouse Cloud AWS region. Defaults to the AWS provider region."
  type        = string
  default     = null
  nullable    = true
}

variable "secret_kms_key_id" {
  description = "Optional customer-managed KMS key ID or ARN for the ClickHouse password secret."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.secret_kms_key_id == null || trimspace(var.secret_kms_key_id) != ""
    error_message = "secret_kms_key_id must be null or a non-empty KMS key ID or ARN."
  }
}

variable "vpc_id" {
  description = "AWS VPC ID that connects to ClickHouse Cloud over PrivateLink."
  type        = string
}

variable "subnet_ids" {
  description = "Private subnet IDs for the ClickHouse Cloud interface endpoint."
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) > 0
    error_message = "subnet_ids must contain at least one subnet."
  }
}

variable "https_port" {
  description = "ClickHouse HTTPS port used by Helmr."
  type        = number
  default     = 8443

  validation {
    condition     = var.https_port > 0 && var.https_port < 65536
    error_message = "https_port must be a valid TCP port."
  }
}

variable "min_replica_memory_gb" {
  description = "Minimum memory per ClickHouse Cloud replica."
  type        = number
  default     = 8
}

variable "max_replica_memory_gb" {
  description = "Maximum memory per ClickHouse Cloud replica."
  type        = number
  default     = 32
}

variable "idle_scaling" {
  description = "Enable ClickHouse Cloud idle scaling."
  type        = bool
  default     = false
}

variable "idle_timeout_minutes" {
  description = "ClickHouse Cloud idle timeout in minutes when idle scaling is enabled."
  type        = number
  default     = 5

  validation {
    condition     = var.idle_timeout_minutes >= 5
    error_message = "idle_timeout_minutes must be at least 5 minutes."
  }
}

variable "release_channel" {
  description = "ClickHouse Cloud release channel."
  type        = string
  default     = "default"

  validation {
    condition     = contains(["default", "fast", "slow"], var.release_channel)
    error_message = "release_channel must be default, fast, or slow."
  }
}

variable "backup_period_in_hours" {
  description = "ClickHouse Cloud backup period in hours."
  type        = number
  default     = 24
}

variable "backup_retention_period_in_hours" {
  description = "ClickHouse Cloud backup retention in hours."
  type        = number
  default     = 24
}

variable "secret_recovery_window_in_days" {
  description = "Secrets Manager recovery window for the ClickHouse password secret."
  type        = number
  default     = 30

  validation {
    condition     = var.secret_recovery_window_in_days == 0 || (var.secret_recovery_window_in_days >= 7 && var.secret_recovery_window_in_days <= 30)
    error_message = "secret_recovery_window_in_days must be 0 for force delete, or between 7 and 30 days."
  }
}

variable "tags" {
  description = "Tags applied to AWS resources."
  type        = map(string)
  default     = {}
}
