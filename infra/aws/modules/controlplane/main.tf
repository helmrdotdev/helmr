locals {
  name                    = lower(var.name)
  controlplane_port       = 8080
  bucket_prefix           = lower(coalesce(var.bucket_name_prefix, "${local.name}-${data.aws_caller_identity.current.account_id}-${data.aws_region.current.region}"))
  controlplane_url        = var.enable_cloudfront ? "https://${aws_cloudfront_distribution.controlplane[0].domain_name}" : var.public_url
  controlplane_subnet_ids = var.controlplane_assign_public_ip ? var.public_subnet_ids : var.private_subnet_ids
  email_from              = var.email_from == null ? "" : var.email_from
  smtp_addr               = var.smtp_addr == null ? "" : var.smtp_addr
  smtp_username           = var.smtp_username == null ? "" : var.smtp_username
  clickhouse_url          = trimspace(var.clickhouse_url)
  clickhouse_user         = var.clickhouse_user == null ? "" : var.clickhouse_user
  region_id               = trimspace(coalesce(var.region_id, data.aws_region.current.region))
  default_region_id       = trimspace(coalesce(var.default_region_id, local.region_id))
  region_display_name     = trimspace(coalesce(var.region_display_name, local.region_id))
  worker_groups_by_id = {
    for group in var.worker_groups : group.id => group
  }
  worker_enrollment_secret_env_by_group = {
    for group in var.worker_groups : group.id => "WORKER_GROUP_ENROLLMENT_SECRET_${upper(substr(sha256(group.id), 0, 16))}"
  }
  worker_groups_config = [for group in var.worker_groups : {
    id                      = group.id
    name                    = group.name
    description             = group.description
    enrollment_secret_env   = local.worker_enrollment_secret_env_by_group[group.id]
    allows_run              = group.allows_run
    allows_build            = group.allows_build
    observation_ttl_seconds = group.observation_ttl_seconds
    instance_capacity       = group.instance_capacity
  }]
  image_cache_repository_prefix     = "${local.name}/image-cache"
  image_cache_repository_arn_prefix = "arn:${data.aws_partition.current.partition}:ecr:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:repository/${local.image_cache_repository_prefix}/"
  image_cache_registry_authority    = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${data.aws_region.current.region}.${data.aws_partition.current.dns_suffix}"
  image_cache_worker_role_arns      = sort(distinct(var.image_cache_worker_role_arns))
  worker_enrollment_secrets = {
    for group_id, environment_name in local.worker_enrollment_secret_env_by_group :
    environment_name => aws_secretsmanager_secret.worker_enrollment[group_id].arn
  }
  secret_kms_key_arns = distinct(concat(
    [aws_kms_key.helmr.arn],
    var.clickhouse_password_kms_key_arns,
    var.operator_token_kms_key_arn == null ? [] : [var.operator_token_kms_key_arn]
  ))
  dispatcher_secret_kms_key_arns = distinct(concat(
    [aws_kms_key.helmr.arn],
    var.clickhouse_password_kms_key_arns
  ))
  controlplane_security_group_ids = concat(
    [aws_security_group.controlplane.id],
    var.additional_controlplane_security_group_ids
  )

  clickhouse_environment = merge({
    CLICKHOUSE_URL = local.clickhouse_url
    }, local.clickhouse_user == "" ? {} : {
    CLICKHOUSE_USER = local.clickhouse_user
  })
  region_environment = {
    REGION_ID           = local.region_id
    DEFAULT_REGION_ID   = local.default_region_id
    PROVIDER            = "aws"
    PROVIDER_REGION     = data.aws_region.current.region
    REGION_DISPLAY_NAME = local.region_display_name
  }

  telemetry_secrets = var.clickhouse_password_secret_arn == null ? {} : {
    CLICKHOUSE_PASSWORD = var.clickhouse_password_secret_arn
  }
  migration_secrets = merge({
    DATABASE_URL = aws_secretsmanager_secret.database_url.arn
  }, local.telemetry_secrets)

  migration_environment = local.clickhouse_environment

  email_environment = merge(
    var.email_provider == "none" ? {} : {
      EMAIL_PROVIDER = var.email_provider
    },
    contains(["smtp", "resend"], var.email_provider) ? {
      EMAIL_FROM = local.email_from
    } : {},
    var.email_provider == "smtp" ? merge({
      SMTP_ADDR = local.smtp_addr
      },
      local.smtp_username == "" ? {} : {
        SMTP_USERNAME = local.smtp_username
      }
    ) : {}
  )

  email_secrets = merge(
    var.email_provider == "resend" ? {
      RESEND_API_KEY = aws_secretsmanager_secret.resend_api_key[0].arn
    } : {},
    var.email_provider == "smtp" && var.smtp_password_enabled ? {
      SMTP_PASSWORD = aws_secretsmanager_secret.smtp_password[0].arn
    } : {}
  )

  controlplane_environment_defaults = merge({
    CONTROL_PLANE_ADDR                = ":${local.controlplane_port}"
    DEPLOYMENT_MODE                   = var.deployment_mode
    CAS_URI                           = "s3://${aws_s3_bucket.cas.bucket}"
    BUILD_POLICY_PATH                 = "/release/build-policy.json"
    PLATFORM_STORE_URI                = var.platform_store_uri
    PUBLIC_URL                        = local.controlplane_url
    REDIS_URL                         = local.redis_url
    SCHEDULE_JITTER                   = var.schedule_jitter
    GITHUB_OAUTH_CLIENT_ID            = var.github_oauth_client_id
    WORKER_GROUPS                     = jsonencode(local.worker_groups_config)
    IMAGE_CACHE_REGISTRY_AUTHORITY    = local.image_cache_registry_authority
    IMAGE_CACHE_REPOSITORY_PREFIX     = local.image_cache_repository_prefix
    IMAGE_CACHE_ROLE_ARN              = aws_iam_role.image_cache.arn
    IMAGE_CACHE_REPOSITORY_ARN_PREFIX = local.image_cache_repository_arn_prefix
  }, local.region_environment, local.clickhouse_environment, local.email_environment)

  controlplane_secret_defaults = merge({
    DATABASE_URL               = aws_secretsmanager_secret.database_url.arn
    WORKER_TOKEN_SIGNING_KEY   = aws_secretsmanager_secret.worker_token_signing_key.arn
    SETUP_TOKEN                = aws_secretsmanager_secret.setup_token.arn
    AUTH_KEY                   = aws_secretsmanager_secret.auth_key.arn
    ENCRYPTION_KEY             = aws_secretsmanager_secret.encryption_key.arn
    WORKSPACE_FENCING_KEY      = aws_secretsmanager_secret.workspace_fencing_key.arn
    TOKEN_CREDENTIAL_KEY       = aws_secretsmanager_secret.token_credential_key.arn
    GITHUB_OAUTH_CLIENT_SECRET = aws_secretsmanager_secret.github_oauth_client_secret.arn
    },
    var.operator_token_secret_arn == null ? {} : {
      OPERATOR_TOKEN = var.operator_token_secret_arn
    },
    local.worker_enrollment_secrets,
    local.telemetry_secrets,
    local.email_secrets
  )

  reserved_optional_controlplane_keys = toset([
    "EMAIL_PROVIDER",
    "EMAIL_FROM",
    "RESEND_API_KEY",
    "SMTP_ADDR",
    "SMTP_USERNAME",
    "SMTP_PASSWORD",
    "REGION_ID",
    "DEFAULT_REGION_ID",
    "PROVIDER",
    "PROVIDER_REGION",
    "REGION_DISPLAY_NAME",
    "CLICKHOUSE_URL",
    "CLICKHOUSE_USER",
    "CLICKHOUSE_PASSWORD",
  ])
  reserved_controlplane_environment_keys = toset(keys(local.controlplane_environment_defaults))
  reserved_controlplane_secret_keys      = setunion(toset(keys(local.controlplane_secret_defaults)), toset(["OPERATOR_TOKEN"]))
  reserved_controlplane_keys             = setunion(local.reserved_controlplane_environment_keys, local.reserved_controlplane_secret_keys, local.reserved_optional_controlplane_keys)
  reserved_dispatcher_keys = setunion(toset(keys(local.dispatcher_environment_defaults)), toset(keys(local.dispatcher_secrets)), toset([
    "CLICKHOUSE_PASSWORD",
  ]))
  controlplane_environment_conflicts = setintersection(keys(var.controlplane_environment), local.reserved_controlplane_keys)
  dispatcher_environment_conflicts   = setintersection(keys(var.dispatcher_environment), local.reserved_dispatcher_keys)
  controlplane_environment           = merge(var.controlplane_environment, local.controlplane_environment_defaults)
  controlplane_secrets               = local.controlplane_secret_defaults

  dispatcher_environment_defaults = merge({
    REDIS_URL              = local.redis_url
    SCHEDULE_POLL_INTERVAL = var.schedule_poll_interval
    SCHEDULE_CLAIM_LIMIT   = tostring(var.schedule_claim_limit)
    SCHEDULE_CONCURRENCY   = tostring(var.schedule_concurrency)
    SCHEDULE_CLAIM_LEASE   = var.schedule_claim_lease
  }, local.clickhouse_environment)
  dispatcher_environment = merge(var.dispatcher_environment, local.dispatcher_environment_defaults)

  dispatcher_secrets = merge({
    DATABASE_URL          = aws_secretsmanager_secret.database_url.arn
    WORKSPACE_FENCING_KEY = aws_secretsmanager_secret.workspace_fencing_key.arn
    }, local.telemetry_secrets
  )

  redis_url = "rediss://${aws_elasticache_replication_group.dispatch.primary_endpoint_address}:${aws_elasticache_replication_group.dispatch.port}/0"
}

resource "terraform_data" "bootstrap_preconditions" {
  input = {
    certificate_arn                   = var.certificate_arn
    cloudfront_origin                 = var.cloudfront_origin_domain_name
    enable_cloudfront                 = var.enable_cloudfront
    public_url                        = var.public_url
    reserved_env_conflicts            = local.controlplane_environment_conflicts
    reserved_dispatcher_env_conflicts = local.dispatcher_environment_conflicts
    email_provider                    = var.email_provider
    platform_store_uri                = var.platform_store_uri
    platform_store_bucket_arn         = var.platform_store_bucket_arn
  }

  lifecycle {
    precondition {
      condition     = length(local.controlplane_environment_conflicts) == 0
      error_message = "controlplane_environment must not set managed Helmr variables. Use explicit module inputs and secret containers for managed settings."
    }

    precondition {
      condition     = length(local.image_cache_worker_role_arns) > 0
      error_message = "image_cache_worker_role_arns must contain at least one deployment-owned build Worker role."
    }

    precondition {
      condition     = length(local.dispatcher_environment_conflicts) == 0
      error_message = "dispatcher_environment must not set managed Helmr variables. Use explicit module inputs and secret containers for managed settings."
    }

    precondition {
      condition     = var.platform_store_uri == "s3://${trimprefix(var.platform_store_bucket_arn, "arn:${data.aws_partition.current.partition}:s3:::")}/objects"
      error_message = "platform_store_uri must identify the bucket supplied by platform_store_bucket_arn and end in /objects."
    }

    precondition {
      condition     = var.platform_store_bucket_arn != aws_s3_bucket.cas.arn
      error_message = "platform_store_bucket_arn must identify the dedicated bootstrap store, not the mutable Control Plane CAS bucket."
    }

    precondition {
      condition = alltrue([
        for group in var.worker_groups :
        trimspace(group.id) != "" && trimspace(group.name) != "" &&
        (group.allows_run || group.allows_build) &&
        group.observation_ttl_seconds > 0 && group.observation_ttl_seconds <= 2592000 &&
        group.instance_capacity.milli_cpu > 0 && group.instance_capacity.memory_bytes > 0 &&
        group.instance_capacity.guest_ephemeral_disk_bytes > 0 &&
        group.instance_capacity.build_cache_bytes >= 0 && group.instance_capacity.artifact_cache_bytes >= 0 &&
        group.instance_capacity.vm_slots >= 0 && group.instance_capacity.build_executors >= 0 &&
        (!group.allows_run || group.instance_capacity.vm_slots > 0) &&
        (!group.allows_build || group.instance_capacity.build_executors > 0)
      ])
      error_message = "worker_groups must define a complete logical role boundary and a positive role-compatible capacity floor."
    }

    precondition {
      condition = alltrue([
        for role_arn in var.image_cache_worker_role_arns :
        startswith(role_arn, "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:role/")
      ])
      error_message = "image_cache_worker_role_arns must identify roles in the Control Plane AWS account."
    }

    precondition {
      condition     = var.email_provider != "none" || (local.email_from == "" && local.smtp_addr == "" && local.smtp_username == "" && !var.smtp_password_enabled)
      error_message = "email_provider=none cannot be combined with email sender settings."
    }

    precondition {
      condition     = var.email_provider != "log" || (local.email_from == "" && local.smtp_addr == "" && local.smtp_username == "" && !var.smtp_password_enabled)
      error_message = "email_provider=log cannot be combined with email sender settings."
    }

    precondition {
      condition     = var.email_provider != "resend" || (trimspace(local.email_from) != "" && local.smtp_addr == "" && local.smtp_username == "" && !var.smtp_password_enabled)
      error_message = "email_provider=resend requires email_from and cannot be combined with SMTP settings."
    }

    precondition {
      condition     = var.email_provider != "smtp" || (trimspace(local.email_from) != "" && trimspace(local.smtp_addr) != "")
      error_message = "email_provider=smtp requires email_from and smtp_addr."
    }

    precondition {
      condition     = var.email_provider != "smtp" || local.smtp_username != "" || !var.smtp_password_enabled
      error_message = "smtp_password_enabled requires smtp_username."
    }

    precondition {
      condition     = var.enable_cloudfront || (var.public_url != null && (startswith(var.public_url, "https://") || (var.allow_insecure_http && startswith(var.public_url, "http://"))))
      error_message = "public_url must be HTTPS when enable_cloudfront is false, unless allow_insecure_http is explicitly enabled for development."
    }

    precondition {
      condition     = !var.enable_cloudfront || (var.certificate_arn != null && var.cloudfront_origin_domain_name != null && trimspace(var.cloudfront_origin_domain_name) != "")
      error_message = "enable_cloudfront requires certificate_arn and cloudfront_origin_domain_name so CloudFront can use a TLS ALB origin without pointing at its own viewer hostname."
    }

  }
}

resource "aws_kms_key" "helmr" {
  description             = "KMS key for Helmr control-plane storage"
  deletion_window_in_days = var.kms_deletion_window_in_days
  enable_key_rotation     = true
  tags                    = var.tags
}

resource "aws_kms_alias" "helmr" {
  name          = "alias/${local.name}"
  target_key_id = aws_kms_key.helmr.key_id
}

resource "aws_s3_bucket" "cas" {
  bucket = "${local.bucket_prefix}-cas"
  tags   = var.tags
}

resource "aws_s3_bucket_public_access_block" "cas" {
  bucket                  = aws_s3_bucket.cas.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "cas" {
  bucket = aws_s3_bucket.cas.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "cas" {
  bucket = aws_s3_bucket.cas.id

  rule {
    apply_server_side_encryption_by_default {
      kms_master_key_id = aws_kms_key.helmr.arn
      sse_algorithm     = "aws:kms"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "cas" {
  count = var.cas_object_expiration_days == null && var.cas_noncurrent_version_expiration_days == null ? 0 : 1

  bucket = aws_s3_bucket.cas.id

  rule {
    id     = "expire-dev-cas-objects"
    status = "Enabled"

    filter {
      tag {
        key   = "helmr-expirable"
        value = "true"
      }
    }

    dynamic "expiration" {
      for_each = var.cas_object_expiration_days == null ? [] : [var.cas_object_expiration_days]

      content {
        days = expiration.value
      }
    }

    dynamic "noncurrent_version_expiration" {
      for_each = var.cas_noncurrent_version_expiration_days == null ? [] : [var.cas_noncurrent_version_expiration_days]

      content {
        noncurrent_days = noncurrent_version_expiration.value
      }
    }
  }
}

resource "aws_db_subnet_group" "postgres" {
  name       = "${local.name}-postgres"
  subnet_ids = var.private_subnet_ids
  tags       = var.tags
}

resource "aws_security_group" "postgres" {
  name        = "${local.name}-postgres"
  description = "Helmr Postgres access"
  vpc_id      = var.vpc_id
  tags        = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "postgres" {
  for_each                     = toset(var.allowed_security_group_ids)
  security_group_id            = aws_security_group.postgres.id
  referenced_security_group_id = each.value
  from_port                    = 5432
  ip_protocol                  = "tcp"
  to_port                      = 5432
}

resource "aws_vpc_security_group_egress_rule" "postgres" {
  security_group_id = aws_security_group.postgres.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_security_group" "redis" {
  name        = "${local.name}-dispatch"
  description = "Helmr Redis/Valkey dispatch access"
  vpc_id      = var.vpc_id
  tags        = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "redis_controlplane" {
  security_group_id            = aws_security_group.redis.id
  referenced_security_group_id = aws_security_group.controlplane.id
  from_port                    = 6379
  ip_protocol                  = "tcp"
  to_port                      = 6379
}

resource "aws_vpc_security_group_egress_rule" "redis" {
  security_group_id = aws_security_group.redis.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_elasticache_subnet_group" "dispatch" {
  name       = "${local.name}-dispatch"
  subnet_ids = var.private_subnet_ids
  tags       = var.tags
}

resource "aws_elasticache_replication_group" "dispatch" {
  replication_group_id       = "${local.name}-dispatch"
  description                = "Helmr dispatch queue and worker lease hot path"
  engine                     = var.redis_engine
  node_type                  = var.redis_node_type
  num_cache_clusters         = var.redis_node_count
  port                       = 6379
  at_rest_encryption_enabled = true
  transit_encryption_enabled = true
  kms_key_id                 = aws_kms_key.helmr.arn
  security_group_ids         = [aws_security_group.redis.id]
  subnet_group_name          = aws_elasticache_subnet_group.dispatch.name
  automatic_failover_enabled = var.redis_node_count > 1
  multi_az_enabled           = var.redis_node_count > 1
  tags                       = var.tags

  lifecycle {
    precondition {
      condition     = var.redis_node_count >= 1
      error_message = "redis_node_count must be at least 1."
    }
  }
}

resource "random_id" "postgres_final_snapshot" {
  byte_length = 4
}

resource "aws_db_instance" "postgres" {
  identifier                   = "${local.name}-postgres"
  engine                       = "postgres"
  engine_version               = var.database_engine_version
  instance_class               = var.database_instance_class
  allocated_storage            = var.database_allocated_storage_gb
  db_name                      = "helmr"
  username                     = "helmr"
  manage_master_user_password  = true
  storage_encrypted            = true
  kms_key_id                   = aws_kms_key.helmr.arn
  db_subnet_group_name         = aws_db_subnet_group.postgres.name
  vpc_security_group_ids       = [aws_security_group.postgres.id]
  multi_az                     = var.database_multi_az
  backup_retention_period      = var.database_backup_retention_days
  deletion_protection          = var.database_deletion_protection
  performance_insights_enabled = var.database_performance_insights_enabled
  skip_final_snapshot          = var.database_skip_final_snapshot
  final_snapshot_identifier    = "${local.name}-postgres-final-${random_id.postgres_final_snapshot.hex}"
  tags                         = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "postgres_controlplane" {
  security_group_id            = aws_security_group.postgres.id
  referenced_security_group_id = aws_security_group.controlplane.id
  from_port                    = 5432
  ip_protocol                  = "tcp"
  to_port                      = 5432
}

resource "aws_cloudwatch_log_group" "controlplane" {
  name              = "/aws/ecs/${local.name}/controlplane"
  retention_in_days = var.controlplane_log_retention_days
  tags              = var.tags
}

data "aws_ec2_managed_prefix_list" "cloudfront_origin" {
  count = var.enable_cloudfront ? 1 : 0

  name = "com.amazonaws.global.cloudfront.origin-facing"
}

data "aws_cloudfront_cache_policy" "caching_disabled" {
  count = var.enable_cloudfront ? 1 : 0

  name = "Managed-CachingDisabled"
}

resource "aws_security_group" "alb" {
  name        = "${local.name}-controlplane-alb"
  description = "Helmr control-plane load balancer"
  vpc_id      = var.vpc_id
  tags        = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "alb_http_public" {
  count             = !var.enable_cloudfront && (var.certificate_arn != null || var.allow_insecure_http) ? 1 : 0
  security_group_id = aws_security_group.alb.id
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 80
  ip_protocol       = "tcp"
  to_port           = 80
}

resource "aws_vpc_security_group_ingress_rule" "alb_https_cloudfront" {
  count             = var.enable_cloudfront ? 1 : 0
  security_group_id = aws_security_group.alb.id
  prefix_list_id    = data.aws_ec2_managed_prefix_list.cloudfront_origin[0].id
  from_port         = 443
  ip_protocol       = "tcp"
  to_port           = 443
}

resource "aws_vpc_security_group_ingress_rule" "alb_https" {
  count             = var.enable_cloudfront || var.certificate_arn == null ? 0 : 1
  security_group_id = aws_security_group.alb.id
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  ip_protocol       = "tcp"
  to_port           = 443
}

resource "aws_vpc_security_group_egress_rule" "alb" {
  security_group_id = aws_security_group.alb.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_security_group" "controlplane" {
  name        = "${local.name}-controlplane"
  description = "Helmr control-plane tasks"
  vpc_id      = var.vpc_id
  tags        = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "controlplane_alb" {
  security_group_id            = aws_security_group.controlplane.id
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = local.controlplane_port
  ip_protocol                  = "tcp"
  to_port                      = local.controlplane_port
}

resource "aws_vpc_security_group_egress_rule" "controlplane" {
  security_group_id = aws_security_group.controlplane.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_lb" "controlplane" {
  name               = "${local.name}-controlplane"
  load_balancer_type = "application"
  internal           = false
  security_groups    = [aws_security_group.alb.id]
  subnets            = var.public_subnet_ids
  tags               = var.tags
}

resource "aws_lb_target_group" "controlplane" {
  name        = "${local.name}-controlplane"
  port        = local.controlplane_port
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = var.vpc_id
  tags        = var.tags

  health_check {
    enabled             = true
    healthy_threshold   = 2
    interval            = 30
    matcher             = "200"
    path                = var.controlplane_health_check_path
    timeout             = 5
    unhealthy_threshold = 2
  }
}

resource "aws_lb_listener" "http" {
  count             = !var.enable_cloudfront && var.allow_insecure_http ? 1 : 0
  load_balancer_arn = aws_lb.controlplane.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.controlplane.arn
  }
}

resource "aws_lb_listener" "http_redirect" {
  count             = var.enable_cloudfront || var.allow_insecure_http || var.certificate_arn == null ? 0 : 1
  load_balancer_arn = aws_lb.controlplane.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "redirect"

    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
}

resource "aws_lb_listener" "https" {
  count             = var.certificate_arn == null ? 0 : 1
  load_balancer_arn = aws_lb.controlplane.arn
  port              = 443
  protocol          = "HTTPS"
  certificate_arn   = var.certificate_arn
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.controlplane.arn
  }
}

resource "aws_cloudfront_origin_request_policy" "controlplane" {
  count = var.enable_cloudfront ? 1 : 0

  name    = "${local.name}-controlplane"
  comment = "Forward dynamic control-plane requests to the Helmr ALB."

  cookies_config {
    cookie_behavior = "all"
  }

  headers_config {
    header_behavior = "allExcept"

    headers {
      items = ["Host"]
    }
  }

  query_strings_config {
    query_string_behavior = "all"
  }
}

resource "aws_cloudfront_distribution" "controlplane" {
  count = var.enable_cloudfront ? 1 : 0

  enabled         = true
  is_ipv6_enabled = true
  comment         = "${local.name} Helmr Control Plane"
  price_class     = "PriceClass_100"
  tags            = var.tags

  origin {
    domain_name = var.cloudfront_origin_domain_name
    origin_id   = "controlplane-alb"

    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "https-only"
      origin_ssl_protocols   = ["TLSv1.2"]
    }
  }

  default_cache_behavior {
    target_origin_id         = "controlplane-alb"
    viewer_protocol_policy   = "redirect-to-https"
    allowed_methods          = ["DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"]
    cached_methods           = ["GET", "HEAD"]
    compress                 = true
    cache_policy_id          = data.aws_cloudfront_cache_policy.caching_disabled[0].id
    origin_request_policy_id = aws_cloudfront_origin_request_policy.controlplane[0].id
  }

  restrictions {
    geo_restriction {
      restriction_type = "none"
    }
  }

  viewer_certificate {
    cloudfront_default_certificate = true
  }

  depends_on = [
    aws_lb_listener.https
  ]
}

resource "aws_ecs_cluster" "controlplane" {
  name = "${local.name}-controlplane"
  tags = var.tags
}

resource "aws_iam_role" "controlplane_execution" {
  name = "${local.name}-controlplane-execution"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ecs-tasks.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role" "dispatcher_execution" {
  name = "${local.name}-dispatcher-execution"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ecs-tasks.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "controlplane_execution" {
  name = "${local.name}-controlplane-execution"
  role = aws_iam_role.controlplane_execution.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "WriteControlPlaneLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.controlplane.arn}:*"
      },
      {
        Effect = "Allow"
        Action = [
          "secretsmanager:GetSecretValue"
        ]
        Resource = values(local.controlplane_secrets)
      },
      {
        Effect = "Allow"
        Action = [
          "kms:Decrypt"
        ]
        Resource = local.secret_kms_key_arns
        Condition = {
          StringEquals = {
            "kms:ViaService" = "secretsmanager.${data.aws_region.current.region}.amazonaws.com"
          }
        }
      }
      ], var.controlplane_image_repository_arn == null ? [] : [
      {
        Sid      = "AuthenticateControlPlaneReleaseRegistry"
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      },
      {
        Sid    = "PullControlPlaneReleaseImage"
        Effect = "Allow"
        Action = [
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer"
        ]
        Resource = var.controlplane_image_repository_arn
      }
    ])
  })
}

resource "aws_iam_role_policy" "dispatcher_execution" {
  name = "${local.name}-dispatcher-execution"
  role = aws_iam_role.dispatcher_execution.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "WriteDispatcherLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.controlplane.arn}:*"
      },
      {
        Effect = "Allow"
        Action = [
          "secretsmanager:GetSecretValue"
        ]
        Resource = values(local.dispatcher_secrets)
      },
      {
        Effect = "Allow"
        Action = [
          "kms:Decrypt"
        ]
        Resource = local.dispatcher_secret_kms_key_arns
        Condition = {
          StringEquals = {
            "kms:ViaService" = "secretsmanager.${data.aws_region.current.region}.amazonaws.com"
          }
        }
      }
      ], var.controlplane_image_repository_arn == null ? [] : [
      {
        Sid      = "AuthenticateControlPlaneReleaseRegistry"
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      },
      {
        Sid    = "PullControlPlaneReleaseImage"
        Effect = "Allow"
        Action = [
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer"
        ]
        Resource = var.controlplane_image_repository_arn
      }
    ])
  })
}

resource "aws_iam_policy" "image_cache_boundary" {
  name = "${local.name}-image-cache-boundary"
  tags = var.tags

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "AuthenticateExecutionCacheRegistry"
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      },
      {
        Sid    = "UseExecutionCacheRepositories"
        Effect = "Allow"
        Action = [
          "ecr:BatchCheckLayerAvailability",
          "ecr:BatchGetImage",
          "ecr:CompleteLayerUpload",
          "ecr:GetDownloadUrlForLayer",
          "ecr:InitiateLayerUpload",
          "ecr:PutImage",
          "ecr:UploadLayerPart"
        ]
        Resource = "${local.image_cache_repository_arn_prefix}environments/*"
      }
    ]
  })
}

resource "aws_iam_role" "image_cache" {
  name                 = "${local.name}-image-cache"
  permissions_boundary = aws_iam_policy.image_cache_boundary.arn
  tags                 = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        AWS = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root"
      }
      Action = "sts:AssumeRole"
      Condition = {
        ArnEquals = {
          "aws:PrincipalArn" = local.image_cache_worker_role_arns
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "image_cache" {
  name = "${local.name}-image-cache"
  role = aws_iam_role.image_cache.id

  policy = aws_iam_policy.image_cache_boundary.policy
}

resource "aws_iam_role" "controlplane_task" {
  name = "${local.name}-controlplane-task"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ecs-tasks.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "controlplane_task" {
  name = "${local.name}-controlplane-task"
  role = aws_iam_role.controlplane_task.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Effect = "Allow"
        Action = [
          "s3:GetObject",
          "s3:PutObject",
          "s3:PutObjectTagging",
          "s3:DeleteObject",
          "s3:AbortMultipartUpload",
          "s3:ListBucket"
        ]
        Resource = [
          aws_s3_bucket.cas.arn,
          "${aws_s3_bucket.cas.arn}/*"
        ]
      },
      {
        Effect = "Allow"
        Action = [
          "kms:Decrypt",
          "kms:Encrypt",
          "kms:GenerateDataKey"
        ]
        Resource = aws_kms_key.helmr.arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = "s3.${data.aws_region.current.region}.amazonaws.com"
          }
        }
      },
      {
        Sid      = "ReadManagedRuntimeObjects"
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = "${var.platform_store_bucket_arn}/objects/sha256/*"
      },
      {
        Sid      = "DecryptManagedRuntimeObjects"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.platform_store_kms_key_arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = "s3.${data.aws_region.current.region}.amazonaws.com"
          }
        }
      },
      ], [for statement in [
        {
          Sid    = "ProvisionExecutionImageCache"
          Effect = "Allow"
          Action = [
            "ecr:CreateRepository",
            "ecr:DeleteRepository",
            "ecr:DescribeRepositories",
            "ecr:ListTagsForResource",
            "ecr:PutImageTagMutability",
            "ecr:PutLifecyclePolicy",
            "ecr:SetRepositoryPolicy",
            "ecr:TagResource",
            "ecr:UntagResource"
          ]
          Resource = "${local.image_cache_repository_arn_prefix}environments/*"
        }
    ] : statement])
  })
}

resource "aws_iam_role" "dispatcher_task" {
  name = "${local.name}-dispatcher-task"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ecs-tasks.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role" "migration_task" {
  name = "${local.name}-migration-task"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ecs-tasks.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_ecs_task_definition" "controlplane" {
  family                   = "${local.name}-controlplane"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.controlplane_cpu)
  memory                   = tostring(var.controlplane_memory)
  execution_role_arn       = aws_iam_role.controlplane_execution.arn
  task_role_arn            = aws_iam_role.controlplane_task.arn
  tags                     = var.tags

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.controlplane_architecture
  }

  volume {
    name = "release-policy"
  }

  container_definitions = jsonencode([
    {
      name       = "release-install"
      image      = var.controlplane_image
      essential  = false
      user       = "0"
      entryPoint = ["helmr-controlplane"]
      command = [
        "release",
        "install",
        "--store",
        var.platform_store_uri,
        "--digest",
        var.build_policy_digest,
        "--output",
        "/release/build-policy.json"
      ]
      mountPoints = [{
        sourceVolume  = "release-policy"
        containerPath = "/release"
        readOnly      = false
      }]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.controlplane.name
          awslogs-region        = data.aws_region.current.region
          awslogs-stream-prefix = "release-install"
        }
      }
    },
    {
      name       = "controlplane"
      image      = var.controlplane_image
      essential  = true
      entryPoint = var.controlplane_entrypoint
      dependsOn = [{
        containerName = "release-install"
        condition     = "SUCCESS"
      }]
      mountPoints = [{
        sourceVolume  = "release-policy"
        containerPath = "/release"
        readOnly      = true
      }]
      portMappings = [{
        containerPort = local.controlplane_port
        hostPort      = local.controlplane_port
        protocol      = "tcp"
      }]
      environment = [
        for key, value in local.controlplane_environment : {
          name  = key
          value = value
        }
      ]
      secrets = [
        for key, value in local.controlplane_secrets : {
          name      = key
          valueFrom = value
        }
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.controlplane.name
          awslogs-region        = data.aws_region.current.region
          awslogs-stream-prefix = "controlplane"
        }
      }
    }
  ])
}

resource "aws_ecs_task_definition" "dispatcher" {
  family                   = "${local.name}-dispatcher"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.controlplane_cpu)
  memory                   = tostring(var.controlplane_memory)
  execution_role_arn       = aws_iam_role.dispatcher_execution.arn
  task_role_arn            = aws_iam_role.dispatcher_task.arn
  tags                     = var.tags

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.controlplane_architecture
  }

  container_definitions = jsonencode([{
    name       = "dispatcher"
    image      = var.controlplane_image
    essential  = true
    entryPoint = ["helmr-dispatcher"]
    environment = [
      for key, value in local.dispatcher_environment : {
        name  = key
        value = value
      }
    ]
    secrets = [
      for key, value in local.dispatcher_secrets : {
        name      = key
        valueFrom = value
      }
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.controlplane.name
        awslogs-region        = data.aws_region.current.region
        awslogs-stream-prefix = "dispatcher"
      }
    }
  }])
}

resource "aws_ecs_task_definition" "migration" {
  family                   = "${local.name}-migration"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = aws_iam_role.controlplane_execution.arn
  task_role_arn            = aws_iam_role.migration_task.arn
  tags                     = var.tags

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.controlplane_architecture
  }

  container_definitions = jsonencode([{
    name       = "migration"
    image      = var.controlplane_image
    essential  = true
    entryPoint = var.controlplane_entrypoint
    command    = ["migrate", "up"]
    environment = [
      for key, value in local.migration_environment : {
        name  = key
        value = value
      }
    ]
    secrets = [
      for key, value in local.migration_secrets : {
        name      = key
        valueFrom = value
      }
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.controlplane.name
        awslogs-region        = data.aws_region.current.region
        awslogs-stream-prefix = "migration"
      }
    }
  }])
}

resource "aws_ecs_service" "controlplane" {
  count = var.create_controlplane_service ? 1 : 0

  name            = "controlplane"
  cluster         = aws_ecs_cluster.controlplane.id
  task_definition = aws_ecs_task_definition.controlplane.arn
  desired_count   = var.controlplane_desired_count
  launch_type     = "FARGATE"
  tags            = var.tags

  network_configuration {
    subnets          = local.controlplane_subnet_ids
    security_groups  = local.controlplane_security_group_ids
    assign_public_ip = var.controlplane_assign_public_ip
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.controlplane.arn
    container_name   = "controlplane"
    container_port   = local.controlplane_port
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  depends_on = [
    aws_lb_listener.http,
    aws_lb_listener.http_redirect,
    aws_lb_listener.https,
    aws_cloudfront_distribution.controlplane
  ]

  lifecycle {
    precondition {
      condition     = var.certificate_arn != null || var.allow_insecure_http
      error_message = "certificate_arn is required when create_controlplane_service is true unless allow_insecure_http is explicitly enabled."
    }

  }
}

resource "aws_ecs_service" "dispatcher" {
  count = var.create_controlplane_service ? 1 : 0

  name            = "dispatcher"
  cluster         = aws_ecs_cluster.controlplane.id
  task_definition = aws_ecs_task_definition.dispatcher.arn
  desired_count   = var.dispatcher_desired_count
  launch_type     = "FARGATE"
  tags            = var.tags

  network_configuration {
    subnets          = local.controlplane_subnet_ids
    security_groups  = local.controlplane_security_group_ids
    assign_public_ip = var.controlplane_assign_public_ip
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

}

resource "aws_secretsmanager_secret" "database_url" {
  name                    = "${local.name}/controlplane/database-url"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "worker_token_signing_key" {
  name                    = "${local.name}/controlplane/worker-token-signing-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "auth_key" {
  name                    = "${local.name}/controlplane/auth-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "setup_token" {
  name                    = "${local.name}/controlplane/setup-token"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "encryption_key" {
  name                    = "${local.name}/controlplane/encryption-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "workspace_fencing_key" {
  name                    = "${local.name}/controlplane/workspace-fencing-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "token_credential_key" {
  name                    = "${local.name}/controlplane/token-credential-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "github_oauth_client_secret" {
  name                    = "${local.name}/controlplane/github-oauth-client-secret"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "resend_api_key" {
  count                   = var.email_provider == "resend" ? 1 : 0
  name                    = "${local.name}/controlplane/resend-api-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "smtp_password" {
  count                   = var.email_provider == "smtp" && var.smtp_password_enabled ? 1 : 0
  name                    = "${local.name}/controlplane/smtp-password"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "checkpoint_encryption_key" {
  name                    = "${local.name}/worker/checkpoint-encryption-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "worker_enrollment" {
  for_each = local.worker_groups_by_id

  name                    = "${local.name}/worker-enrollment/${substr(sha256(each.key), 0, 16)}"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}
