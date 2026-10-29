variable "enable_deployment_rollback" {
  description = "Allow ECS to restore the previous task definition on deployment failure. Disable before migrations incompatible with predecessor code; this does not restore the database."
  type        = bool
  default     = true
  nullable    = false
}



variable "permissions_boundary_arn" {
  description = "Optional caller-owned IAM ceiling; does not grant resource permissions."
  type        = string
  default     = null
  validation {
    condition     = var.permissions_boundary_arn == null || can(regex("^arn:aws[a-zA-Z-]*:iam::[0-9]{12}:policy/.+$", var.permissions_boundary_arn))
    error_message = "permissions_boundary_arn must be null or a customer-managed IAM policy ARN."
  }
}

variable "name" {
  description = "Name prefix for control-plane resources."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,17}[a-z0-9]$", var.name))
    error_message = "name must be 3-19 characters so names with the -controlplane suffix fit AWS limits; it must start with a lowercase letter, end with a lowercase letter or number, and contain only lowercase letters, numbers, and hyphens."
  }
}

variable "bucket_name_prefix" {
  description = "Globally unique prefix for S3 buckets. Defaults to name-account-region."
  type        = string
  default     = null
  nullable    = true
}

variable "vpc_id" {
  description = "VPC ID."
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnet IDs for control-plane tasks and Postgres."
  type        = list(string)
}

variable "public_subnet_ids" {
  description = "Public subnet IDs for the control-plane load balancer."
  type        = list(string)
}

variable "app_origin" {
  description = "External HTTPS URL for the direct ALB Control Plane when enable_cloudfront is false."
  type        = string
  default     = null
  nullable    = true
}


variable "deployment_mode" {
  description = "Helmr deployment mode passed to control-plane tasks."
  type        = string
  default     = "self-hosted"

  validation {
    condition     = contains(["self-hosted", "managed-cloud"], var.deployment_mode)
    error_message = "deployment_mode must be self-hosted or managed-cloud."
  }
}

variable "computer_wrapping_key_id" {
  description = "Stable logical identity of the self-hosted Computer root key. Preserve this ID and the root key value with database backups; changing either does not rewrap existing keys."
  type        = string
  default     = "computer-root-v1"
  nullable    = false

  validation {
    condition     = can(regex("^[A-Za-z0-9][A-Za-z0-9._:/-]*$", var.computer_wrapping_key_id)) && length(var.computer_wrapping_key_id) <= 2048
    error_message = "computer_wrapping_key_id must be 1–2048 ASCII letters, digits, dots, underscores, colons, slashes or hyphens, starting with a letter or digit."
  }
}

variable "bootstrap_enabled" {
  description = "Create the initial Region and Worker Group when they do not exist."
  type        = bool
  default     = true
}

variable "bootstrap_worker_group_name" {
  description = "Immutable Region-scoped name of the initial Worker Group."
  type        = string
  default     = "default"

  validation {
    condition     = can(regex("^[a-z0-9](?:[a-z0-9-]{0,126}[a-z0-9])?$", var.bootstrap_worker_group_name))
    error_message = "bootstrap_worker_group_name must be a lowercase URL-safe identifier of 1-128 characters."
  }
}

variable "bootstrap_region_id" {
  description = "ID of the initial Region. Defaults to the AWS provider region."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition = var.bootstrap_region_id == null ? true : (
      var.bootstrap_region_id != "" &&
      var.bootstrap_region_id == trimspace(var.bootstrap_region_id) &&
      length(base64encode(var.bootstrap_region_id)) <= 340 &&
      length(regexall("[[:cntrl:]]", var.bootstrap_region_id)) == 0
    )
    error_message = "bootstrap_region_id must be null or normalized control-free UTF-8 of 1-255 bytes."
  }
}

variable "bootstrap_region_display_name" {
  description = "Display name of the initial Region. Defaults to bootstrap_region_id."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.bootstrap_region_display_name == null || trimspace(var.bootstrap_region_display_name) != ""
    error_message = "bootstrap_region_display_name must be null or non-empty."
  }
}

variable "clickhouse_url" {
  description = "ClickHouse HTTP endpoint for historical telemetry."
  type        = string

  validation {
    condition     = can(regex("^https://[^<>[:space:]]+(:[0-9]+)?/?$", trimspace(var.clickhouse_url)))
    error_message = "clickhouse_url must be an https URL without placeholder characters."
  }
}

variable "clickhouse_access_mode" {
  description = "Use bootstrap to provision dedicated ClickHouse users, or external for users provisioned separately."
  type        = string
  validation {
    condition     = contains(["bootstrap", "external"], var.clickhouse_access_mode)
    error_message = "clickhouse_access_mode must be bootstrap or external."
  }
}

variable "clickhouse_reader_user" {
  description = "Explicit ClickHouse reader username."
  type        = string
  nullable    = false
  validation {
    condition     = can(regex("^[A-Za-z_][A-Za-z0-9_]{0,63}$", var.clickhouse_reader_user))
    error_message = "clickhouse_reader_user must be a simple non-empty SQL identifier."
  }
}

variable "clickhouse_reader_password_secret_arn" {
  description = "Secrets Manager ARN containing the ClickHouse reader password."
  type        = string
  nullable    = false
  validation {
    condition     = can(regex("^arn:aws[a-zA-Z-]*:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", var.clickhouse_reader_password_secret_arn))
    error_message = "clickhouse_reader_password_secret_arn must be a Secrets Manager secret ARN."
  }
}

variable "clickhouse_reader_password_kms_key_arns" {
  description = "Optional customer-managed KMS key ARNs for the ClickHouse reader secret."
  type        = list(string)
  default     = []
  validation {
    condition     = alltrue([for arn in var.clickhouse_reader_password_kms_key_arns : trimspace(arn) != ""])
    error_message = "clickhouse_reader_password_kms_key_arns entries must be non-empty."
  }
}

variable "clickhouse_ingester_user" {
  description = "Explicit ClickHouse ingester username."
  type        = string
  nullable    = false
  validation {
    condition     = can(regex("^[A-Za-z_][A-Za-z0-9_]{0,63}$", var.clickhouse_ingester_user))
    error_message = "clickhouse_ingester_user must be a simple non-empty SQL identifier."
  }
}

variable "clickhouse_ingester_password_secret_arn" {
  description = "Secrets Manager ARN containing the ClickHouse ingester password."
  type        = string
  nullable    = false
  validation {
    condition     = can(regex("^arn:aws[a-zA-Z-]*:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", var.clickhouse_ingester_password_secret_arn))
    error_message = "clickhouse_ingester_password_secret_arn must be a Secrets Manager secret ARN."
  }
}

variable "clickhouse_ingester_password_kms_key_arns" {
  description = "Optional customer-managed KMS key ARNs for the ClickHouse ingester secret."
  type        = list(string)
  default     = []
  validation {
    condition     = alltrue([for arn in var.clickhouse_ingester_password_kms_key_arns : trimspace(arn) != ""])
    error_message = "clickhouse_ingester_password_kms_key_arns entries must be non-empty."
  }
}

variable "clickhouse_migration_user" {
  description = "Explicit ClickHouse migration username."
  type        = string
  nullable    = false
  validation {
    condition     = can(regex("^[A-Za-z_][A-Za-z0-9_]{0,63}$", var.clickhouse_migration_user))
    error_message = "clickhouse_migration_user must be a simple non-empty SQL identifier."
  }
}

variable "clickhouse_migration_password_secret_arn" {
  description = "Secrets Manager ARN containing the ClickHouse migration password."
  type        = string
  nullable    = false
  validation {
    condition     = can(regex("^arn:aws[a-zA-Z-]*:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", var.clickhouse_migration_password_secret_arn))
    error_message = "clickhouse_migration_password_secret_arn must be a Secrets Manager secret ARN."
  }
}

variable "clickhouse_migration_password_kms_key_arns" {
  description = "Optional customer-managed KMS key ARNs for the ClickHouse migration secret."
  type        = list(string)
  default     = []
  validation {
    condition     = alltrue([for arn in var.clickhouse_migration_password_kms_key_arns : trimspace(arn) != ""])
    error_message = "clickhouse_migration_password_kms_key_arns entries must be non-empty."
  }
}

variable "clickhouse_bootstrap_user" {
  description = "Explicit ClickHouse bootstrap username."
  type        = string
  default     = null
  nullable    = true
  validation {
    condition     = var.clickhouse_bootstrap_user == null || can(regex("^[A-Za-z_][A-Za-z0-9_]{0,63}$", var.clickhouse_bootstrap_user))
    error_message = "clickhouse_bootstrap_user must be a simple non-empty SQL identifier."
  }
}

variable "clickhouse_bootstrap_password_secret_arn" {
  description = "Secrets Manager ARN containing the ClickHouse bootstrap password."
  type        = string
  default     = null
  nullable    = true
  validation {
    condition     = var.clickhouse_bootstrap_password_secret_arn == null || can(regex("^arn:aws[a-zA-Z-]*:secretsmanager:[^:]+:[0-9]{12}:secret:.+$", var.clickhouse_bootstrap_password_secret_arn))
    error_message = "clickhouse_bootstrap_password_secret_arn must be a Secrets Manager secret ARN."
  }
}

variable "clickhouse_bootstrap_password_kms_key_arns" {
  description = "Optional customer-managed KMS key ARNs for the ClickHouse bootstrap secret."
  type        = list(string)
  default     = []
  validation {
    condition     = alltrue([for arn in var.clickhouse_bootstrap_password_kms_key_arns : trimspace(arn) != ""])
    error_message = "clickhouse_bootstrap_password_kms_key_arns entries must be non-empty."
  }
}

variable "controlplane_image" {
  description = "Container image URI containing control-plane, dispatcher, and deployment tooling. Managed release flows should pass a digest-pinned image."
  type        = string

  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.controlplane_image))
    error_message = "controlplane_image must be pinned by a lowercase sha256 digest."
  }
}

variable "controlplane_image_repository_arn" {
  description = "Exact ECR repository ARN from which ECS task execution roles may pull the Control Plane image. Leave null only for a non-ECR self-hosted image."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition = (
      var.controlplane_image_repository_arn == null ||
      can(regex("^arn:[^:]+:ecr:[a-z0-9-]+:[0-9]{12}:repository/[a-z0-9._/-]+$", var.controlplane_image_repository_arn))
    )
    error_message = "controlplane_image_repository_arn must be null or an ECR repository ARN."
  }
}

variable "platform_store_uri" {
  description = "Dedicated immutable Platform Artifact store URI ending in /objects."
  type        = string

  validation {
    condition     = can(regex("^s3://[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]/objects$", var.platform_store_uri))
    error_message = "platform_store_uri must be an S3 bucket URI ending exactly in /objects."
  }
}

variable "platform_store_bucket_arn" {
  description = "S3 bucket ARN backing platform_store_uri."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:s3:::[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.platform_store_bucket_arn))
    error_message = "platform_store_bucket_arn must be an S3 bucket ARN."
  }
}

variable "platform_store_kms_key_arn" {
  description = "KMS key ARN used by the dedicated Platform Artifact store."
  type        = string

  validation {
    condition     = can(regex("^arn:[^:]+:kms:[^:]+:[0-9]{12}:key/[0-9a-fA-F-]+$", var.platform_store_kms_key_arn))
    error_message = "platform_store_kms_key_arn must be a KMS key ARN."
  }
}

variable "controlplane_entrypoint" {
  description = "Container entrypoint for the Control Plane."
  type        = list(string)
  default     = ["/usr/local/bin/control-plane"]
}

variable "controlplane_cpu" {
  description = "Fargate CPU units for helmr-controlplane."
  type        = number
  default     = 512
}

variable "controlplane_memory" {
  description = "Fargate memory in MiB for helmr-controlplane."
  type        = number
  default     = 1024
}

variable "controlplane_architecture" {
  description = "Fargate CPU architecture for helmr-controlplane."
  type        = string
  default     = "X86_64"

  validation {
    condition     = contains(["X86_64", "ARM64"], var.controlplane_architecture)
    error_message = "controlplane_architecture must be X86_64 or ARM64."
  }
}

variable "controlplane_desired_count" {
  description = "Desired helmr-controlplane task count."
  type        = number
  default     = 2
}

variable "dispatcher_desired_count" {
  description = "Desired helmr-dispatcher task count."
  type        = number
  default     = 1
}

variable "controlplane_assign_public_ip" {
  description = "Assign public IPs and run controlplane/migration Fargate tasks in public subnets. Useful for dev stacks without NAT Gateway."
  type        = bool
  default     = false
}

variable "create_controlplane_service" {
  description = "Create the ECS service. Keep false until image, secrets, and migrations are ready."
  type        = bool
  default     = false
}

variable "capacity_token_secret_arn" {
  description = "Optional externally owned Secrets Manager ARN containing the provider-neutral capacity credential."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.capacity_token_secret_arn == null || can(regex("^arn:[^:]+:secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]+$", var.capacity_token_secret_arn))
    error_message = "capacity_token_secret_arn must be a Secrets Manager secret ARN."
  }
}

variable "capacity_token_kms_key_arn" {
  description = "Optional KMS key ARN required to decrypt capacity_token_secret_arn."
  type        = string
  default     = null
  nullable    = true

  validation {
    condition     = var.capacity_token_kms_key_arn == null || can(regex("^arn:[^:]+:kms:[a-z0-9-]+:[0-9]{12}:key/[0-9a-f-]+$", var.capacity_token_kms_key_arn))
    error_message = "capacity_token_kms_key_arn must be a KMS key ARN."
  }
}

variable "controlplane_environment" {
  description = "Additional non-secret environment variables for helmr-controlplane. Managed Helmr variables such as APP_ORIGIN are owned by this module."
  type        = map(string)
  default     = {}
  validation {
    condition = alltrue([for name in ["ENVIRONMENT_MAX_RESIDENT_COMPUTERS", "ENVIRONMENT_MAX_CPU_MILLIS", "ENVIRONMENT_MAX_MEMORY_BYTES", "ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES", "ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS", "ENVIRONMENT_MAX_CAUSAL_DEPTH", "ENVIRONMENT_ADMISSION_RATE_PER_SECOND", "ENVIRONMENT_ADMISSION_BURST", "ENVIRONMENT_PREPARATION_TIMEOUT_MS"] :
      can(regex("^[0-9]+$", var.controlplane_environment[name])) &&
      try(tonumber(var.controlplane_environment[name]) > 0 && tonumber(var.controlplane_environment[name]) <= 9223372036854775807, false)
    ])
    error_message = "controlplane_environment must explicitly set every ENVIRONMENT execution limit to a positive integer; see the Environment execution policy documentation."
  }
  validation {
    condition = !var.create_controlplane_service || var.controlplane_desired_count == 0 || alltrue([for name in ["DIAGNOSTIC_CHUNK_BYTES", "DIAGNOSTIC_SOURCE_BYTES", "DIAGNOSTIC_SOURCE_RECORDS", "DIAGNOSTIC_ENVIRONMENT_BYTES", "DIAGNOSTIC_ENVIRONMENT_RECORDS", "DIAGNOSTIC_QUEUE_BYTES", "DIAGNOSTIC_QUEUE_RECORDS", "DIAGNOSTIC_DB_MAX_CONNECTIONS"] :
      can(regex("^[0-9]+$", var.controlplane_environment[name])) &&
      try(tonumber(var.controlplane_environment[name]) > 0 && tonumber(var.controlplane_environment[name]) <= 9223372036854775807, false)
    ])
    error_message = "controlplane_environment must explicitly set every required diagnostic limit to a positive integer before starting the service."
  }
  validation {
    condition = !var.create_controlplane_service || var.controlplane_desired_count == 0 || (
      alltrue([for name in ["DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY", "DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY"] :
        can(regex("^[0-9]+$", var.controlplane_environment[name])) && try(tonumber(var.controlplane_environment[name]) > 0 && tonumber(var.controlplane_environment[name]) <= 2147483647, false)
      ]) &&
      try(tonumber(var.controlplane_environment["DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY"]) <= tonumber(var.controlplane_environment["DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY"]), false) &&
      alltrue([for name in ["DEPLOYMENT_UPLOAD_IDLE_TIMEOUT", "DEPLOYMENT_UPLOAD_MAX_DURATION"] : try(trimspace(var.controlplane_environment[name]) != "", false)])
    )
    error_message = "controlplane_environment must set positive per-process upload capacities (organization <= process) and explicit upload idle/maximum durations before starting the service."
  }
}

variable "dispatcher_environment" {
  description = "Additional non-secret environment variables for helmr-dispatcher. Managed Helmr variables are owned by this module."
  type        = map(string)
  default     = {}
  validation {
    condition = !var.create_controlplane_service || var.dispatcher_desired_count == 0 || alltrue([for name in ["DIAGNOSTIC_EXPORT_BATCH_BYTES", "DIAGNOSTIC_EXPORT_BATCH_RECORDS", "DIAGNOSTIC_CHUNK_BYTES", "DIAGNOSTIC_SOURCE_BYTES", "DIAGNOSTIC_SOURCE_RECORDS", "DIAGNOSTIC_ENVIRONMENT_BYTES", "DIAGNOSTIC_ENVIRONMENT_RECORDS", "DIAGNOSTIC_QUEUE_BYTES", "DIAGNOSTIC_QUEUE_RECORDS", "DIAGNOSTIC_DB_MAX_CONNECTIONS"] :
      can(regex("^[0-9]+$", var.dispatcher_environment[name])) &&
      try(tonumber(var.dispatcher_environment[name]) > 0 && tonumber(var.dispatcher_environment[name]) <= 9223372036854775807, false)
    ])
    error_message = "dispatcher_environment must explicitly set every required diagnostic limit to a positive integer before starting the service."
  }
}

variable "email_provider" {
  description = "Email delivery provider for sign-in codes and run wait notifications."
  type        = string
  default     = "none"

  validation {
    condition     = contains(["none", "log", "smtp", "resend"], var.email_provider)
    error_message = "email_provider must be one of none, log, smtp, or resend."
  }
}

variable "email_from" {
  description = "Sender address for email delivery, such as Helmr <noreply@example.com>."
  type        = string
  default     = null
  nullable    = true
}

variable "smtp_addr" {
  description = "SMTP host:port when email_provider is smtp."
  type        = string
  default     = null
  nullable    = true
}

variable "smtp_username" {
  description = "SMTP username when email_provider is smtp."
  type        = string
  default     = null
  nullable    = true
}

variable "smtp_password_enabled" {
  description = "Create and inject an SMTP password secret when email_provider is smtp."
  type        = bool
  default     = false
}

variable "github_oauth_client_id" {
  description = "GitHub OAuth application client ID; empty disables GitHub login."
  type        = string
  default     = ""
}

variable "certificate_arn" {
  description = "ACM certificate ARN for HTTPS."
  type        = string
  default     = null
  nullable    = true
}

variable "allow_insecure_http" {
  description = "Allow an internet-facing plaintext HTTP forwarding listener. Intended for development only; when certificate_arn is set, false redirects HTTP to HTTPS."
  type        = bool
  default     = false
}

variable "enable_cloudfront" {
  description = "Create a CloudFront distribution with the default cloudfront.net viewer certificate in front of an HTTPS control-plane ALB origin."
  type        = bool
  default     = false
}

variable "cloudfront_origin_domain_name" {
  description = "DNS name CloudFront uses for the HTTPS ALB origin. This must resolve to the public ALB and be covered by certificate_arn."
  type        = string
  default     = null
  nullable    = true
}

variable "database_instance_class" {
  description = "RDS Postgres instance class."
  type        = string
  default     = "db.t4g.micro"
}

variable "database_allocated_storage_gb" {
  description = "RDS allocated storage in GiB."
  type        = number
  default     = 20
}

variable "database_multi_az" {
  description = "Create a standby RDS instance in another Availability Zone."
  type        = bool
  default     = false
}

variable "database_backup_retention_days" {
  description = "RDS automated backup retention in days."
  type        = number
  default     = 7
}

variable "database_performance_insights_enabled" {
  description = "Enable RDS Performance Insights when supported by the chosen instance class."
  type        = bool
  default     = false
}

variable "database_deletion_protection" {
  description = "Protect the RDS instance from accidental deletion."
  type        = bool
  default     = true
}

variable "database_skip_final_snapshot" {
  description = "Skip the final RDS snapshot on destroy. Intended for ephemeral development stacks."
  type        = bool
  default     = false
}

variable "controlplane_log_retention_days" {
  description = "CloudWatch Logs retention in days for controlplane and migration tasks."
  type        = number
  default     = 30
}

variable "kms_deletion_window_in_days" {
  description = "KMS key deletion window in days."
  type        = number
  default     = 30
}

variable "secret_recovery_window_in_days" {
  description = "Secrets Manager recovery window in days. Use 7 for ephemeral dev stacks."
  type        = number
  default     = 30
}

variable "cas_object_expiration_days" {
  description = "Days before current CAS objects expire. Set null to disable current object expiration."
  type        = number
  default     = null
  nullable    = true
}

variable "cas_noncurrent_version_expiration_days" {
  description = "Days before noncurrent CAS object versions expire. Set null to disable noncurrent version expiration."
  type        = number
  default     = null
  nullable    = true
}

variable "allowed_security_group_ids" {
  description = "Security groups allowed to connect to Postgres."
  type        = list(string)
  default     = []
}

variable "additional_controlplane_security_group_ids" {
  description = "Additional security groups attached to controlplane, dispatcher, and migration tasks."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for id in var.additional_controlplane_security_group_ids : trimspace(id) != ""])
    error_message = "additional_controlplane_security_group_ids entries must be non-empty security group IDs."
  }
}

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default     = {}
}

variable "ingress_mode" {
  description = "Application load balancer exposure; private requires explicit client sources and HTTPS."
  type        = string
  default     = "public"
  validation {
    condition     = contains(["public", "private"], var.ingress_mode)
    error_message = "ingress_mode must be public or private."
  }
}

variable "private_client_ipv4_cidrs" {
  description = "Routed company client CIDRs allowed to the internal HTTPS listener."
  type        = set(string)
  default     = []
  validation {
    condition     = alltrue([for cidr in var.private_client_ipv4_cidrs : can(cidrnetmask(cidr)) && try(tonumber(split("/", cidr)[1]) > 0, false)])
    error_message = "Private client sources must be valid IPv4 CIDRs narrower than the default route."
  }
}

variable "private_client_security_group_ids" {
  description = "Security groups allowed to the internal HTTPS listener, including worker hosts and platform callers."
  type        = map(string)
  default     = {}
}

variable "quarantine_upload_vpc_endpoint_id" {
  description = "Control Plane S3 gateway endpoint required for authored-input quarantine writes with private ingress."
  type        = string
  default     = null
  validation {
    condition     = var.quarantine_upload_vpc_endpoint_id == null || can(regex("^vpce-[a-z0-9]+$", var.quarantine_upload_vpc_endpoint_id))
    error_message = "quarantine_upload_vpc_endpoint_id must be an S3 VPC endpoint ID."
  }
}

variable "restrict_platform_egress" {
  description = "Omit general task egress. Caller must attach exact service/proxy rules to the exported task security group."
  type        = bool
  default     = false
}

variable "slack_transport" {
  description = "Deployment-selected Slack delivery; organization admins cannot change it."
  type        = string
  default     = "http"
  validation {
    condition     = contains(["disabled", "http", "socket"], var.slack_transport)
    error_message = "slack_transport must be disabled, http or socket."
  }
}

variable "outbound_proxy_url" {
  description = "Optional HTTP CONNECT proxy origin used by external platform integrations. Keep authentication in the network boundary, not this non-secret input."
  type        = string
  default     = ""
  validation {
    condition     = var.outbound_proxy_url == "" || can(regex("^https?://[A-Za-z0-9.-]+(:[0-9]+)?$", var.outbound_proxy_url))
    error_message = "outbound_proxy_url must be empty or an HTTP(S) origin without credentials, path, query or fragment."
  }
}

variable "audit_retention_seconds" {
  description = "Explicit positive security audit retention policy in seconds."
  type        = number
  validation {
    condition     = var.audit_retention_seconds > 0 && floor(var.audit_retention_seconds) == var.audit_retention_seconds
    error_message = "audit_retention_seconds must be a positive integer."
  }
}

variable "audit_purge_interval_minutes" {
  description = "Interval for bounded audit purge; size with retention volume and replica count."
  type        = number
  default     = 15
  validation {
    condition     = var.audit_purge_interval_minutes >= 2 && var.audit_purge_interval_minutes <= 720 && floor(var.audit_purge_interval_minutes) == var.audit_purge_interval_minutes
    error_message = "audit_purge_interval_minutes must be an integer between 2 and 720."
  }
}
variable "audit_maintenance_timeout_seconds" {
  type    = number
  default = 300
  validation {
    condition     = var.audit_maintenance_timeout_seconds >= 1 && var.audit_maintenance_timeout_seconds <= 3600 && floor(var.audit_maintenance_timeout_seconds) == var.audit_maintenance_timeout_seconds
    error_message = "audit_maintenance_timeout_seconds must be an integer between 1 and 3600."
  }
}
variable "audit_purge_max_batches" {
  type    = number
  default = 50
  validation {
    condition     = var.audit_purge_max_batches >= 1 && var.audit_purge_max_batches <= 1000 && floor(var.audit_purge_max_batches) == var.audit_purge_max_batches
    error_message = "audit_purge_max_batches must be an integer between 1 and 1000."
  }
}
variable "audit_maintenance_alarm_actions" {
  description = "Existing CloudWatch alarm action ARNs for missing audit purge success."
  type        = list(string)
  default     = []
}

variable "network_private_business_cidrs" {
  description = "Canonical RFC1918 business destination ceiling; excludes management prefixes and defaults to no private connectivity."
  type        = list(string)
  default     = []
  validation {
    condition = length(distinct(var.network_private_business_cidrs)) == length(var.network_private_business_cidrs) && alltrue([
      for cidr in var.network_private_business_cidrs : can(cidrnetmask(cidr)) && try(cidrhost(cidr, 0) == split("/", cidr)[0], false) && anytrue([
        for private in ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"] : try(tonumber(split("/", cidr)[1]) >= tonumber(split("/", private)[1]) && cidrhost(format("%s/%s", split("/", cidr)[0], split("/", private)[1]), 0) == split("/", private)[0], false)
      ])
    ])
    error_message = "network_private_business_cidrs must contain unique canonical RFC1918 CIDRs."
  }
  validation {
    condition     = alltrue(flatten([for business in var.network_private_business_cidrs : [for blocked in var.network_blocked_ipv4_cidrs : try(cidrhost(format("%s/%s", split("/", business)[0], min(tonumber(split("/", business)[1]), tonumber(split("/", blocked)[1]))), 0) != cidrhost(format("%s/%s", split("/", blocked)[0], min(tonumber(split("/", business)[1]), tonumber(split("/", blocked)[1]))), 0), false)]]))
    error_message = "Private business ranges must not overlap mandatory management denies."
  }
  validation {
    # Zero padding compares numeric address first, then prefix length, as the runtime does.
    condition = try(
      join(",", [for cidr in var.network_private_business_cidrs : "${join(".", [for octet in split(".", split("/", cidr)[0]) : format("%03d", tonumber(octet))])}/${format("%02d", tonumber(split("/", cidr)[1]))}"]) ==
      join(",", sort([for cidr in var.network_private_business_cidrs : "${join(".", [for octet in split(".", split("/", cidr)[0]) : format("%03d", tonumber(octet))])}/${format("%02d", tonumber(split("/", cidr)[1]))}"])), false
    )
    error_message = "network_private_business_cidrs must be ordered by numeric IPv4 address, then prefix length."
  }
}

variable "network_blocked_ipv4_cidrs" {
  description = "Canonical mandatory management destination denies shared with execution Workers."
  type        = list(string)
  default     = []
  validation {
    condition     = length(distinct(var.network_blocked_ipv4_cidrs)) == length(var.network_blocked_ipv4_cidrs) && alltrue([for cidr in var.network_blocked_ipv4_cidrs : can(cidrnetmask(cidr)) && try(cidrhost(cidr, 0) == split("/", cidr)[0], false)])
    error_message = "network_blocked_ipv4_cidrs must contain unique canonical IPv4 CIDRs."
  }
  validation {
    # Zero padding compares numeric address first, then prefix length, as the runtime does.
    condition = try(
      join(",", [for cidr in var.network_blocked_ipv4_cidrs : "${join(".", [for octet in split(".", split("/", cidr)[0]) : format("%03d", tonumber(octet))])}/${format("%02d", tonumber(split("/", cidr)[1]))}"]) ==
      join(",", sort([for cidr in var.network_blocked_ipv4_cidrs : "${join(".", [for octet in split(".", split("/", cidr)[0]) : format("%03d", tonumber(octet))])}/${format("%02d", tonumber(split("/", cidr)[1]))}"])), false
    )
    error_message = "network_blocked_ipv4_cidrs must be ordered by numeric IPv4 address, then prefix length."
  }
}

variable "controlplane_idle_timeout_seconds" {
  description = "ALB connection idle timeout; keep it above client/server upload inactivity limits. Backend HTTP keepalive is configured 30 seconds longer."
  type        = number
  default     = 180
  validation {
    condition     = var.controlplane_idle_timeout_seconds >= 1 && var.controlplane_idle_timeout_seconds <= 4000 && floor(var.controlplane_idle_timeout_seconds) == var.controlplane_idle_timeout_seconds
    error_message = "controlplane_idle_timeout_seconds must be an integer from 1 through 4000."
  }
}
