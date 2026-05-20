locals {
  name                = lower(var.name)
  control_port        = 8080
  bucket_prefix       = lower(coalesce(var.bucket_name_prefix, "${local.name}-${data.aws_caller_identity.current.account_id}-${data.aws_region.current.region}"))
  control_url         = var.enable_cloudfront ? "https://${aws_cloudfront_distribution.control[0].domain_name}" : var.public_url
  private_control_url = var.private_control_dns_name == null ? null : "https://${var.private_control_dns_name}"
  control_subnet_ids  = var.control_assign_public_ip ? var.public_subnet_ids : var.private_subnet_ids

  bootstrap_environment = trimspace(var.bootstrap_owner_email) == "" ? {} : {
    HELMR_BOOTSTRAP_OWNER_EMAIL = var.bootstrap_owner_email
  }

  managed_control_environment = {
    HELMR_CONTROL_ADDR         = ":${local.control_port}"
    HELMR_CAS_URI              = "s3://${aws_s3_bucket.cas.bucket}"
    HELMR_PUBLIC_URL           = local.control_url
    HELMR_REDIS_URL            = local.redis_url
    HELMR_SETUP_ENABLED        = tostring(var.setup_enabled)
    HELMR_GITHUB_APP_ID        = var.github_app_id
    HELMR_GITHUB_APP_SLUG      = var.github_app_slug
    HELMR_GITHUB_APP_CLIENT_ID = var.github_app_client_id
  }

  reserved_control_environment_keys = toset(concat(keys(local.managed_control_environment), ["HELMR_BOOTSTRAP_OWNER_EMAIL"]))
  control_environment_conflicts     = setintersection(keys(var.control_environment), local.reserved_control_environment_keys)
  control_environment               = merge(var.control_environment, local.managed_control_environment, local.bootstrap_environment)

  control_secrets = {
    HELMR_DATABASE_URL              = aws_secretsmanager_secret.database_url.arn
    HELMR_WORKER_TOKEN_SIGNING_KEY  = aws_secretsmanager_secret.worker_token_signing_key.arn
    HELMR_WORKER_REGISTRATION_TOKEN = aws_secretsmanager_secret.worker_registration_token.arn
    HELMR_AUTH_SECRET               = aws_secretsmanager_secret.auth_secret.arn
    HELMR_SECRET_ENCRYPTION_KEY     = aws_secretsmanager_secret.secret_encryption_key.arn
    HELMR_GITHUB_APP_PRIVATE_KEY    = aws_secretsmanager_secret.github_app_private_key.arn
    HELMR_GITHUB_APP_WEBHOOK_SECRET = aws_secretsmanager_secret.github_app_webhook_secret.arn
    HELMR_GITHUB_APP_CLIENT_SECRET  = aws_secretsmanager_secret.github_app_client_secret.arn
  }

  dispatcher_environment = {
    HELMR_REDIS_URL = local.redis_url
  }

  dispatcher_secrets = {
    HELMR_DATABASE_URL = aws_secretsmanager_secret.database_url.arn
  }

  redis_url = "rediss://${aws_elasticache_replication_group.dispatch.primary_endpoint_address}:${aws_elasticache_replication_group.dispatch.port}/0"
}

data "aws_vpc" "control" {
  id = var.vpc_id
}

resource "terraform_data" "bootstrap_preconditions" {
  input = {
    setup_enabled          = var.setup_enabled
    bootstrap_owner_email  = var.bootstrap_owner_email
    certificate_arn        = var.certificate_arn
    cloudfront_origin      = var.cloudfront_origin_domain_name
    enable_cloudfront      = var.enable_cloudfront
    private_control_dns    = var.private_control_dns_name
    public_url             = var.public_url
    reserved_env_conflicts = local.control_environment_conflicts
  }

  lifecycle {
    precondition {
      condition     = !var.setup_enabled || trimspace(var.bootstrap_owner_email) != ""
      error_message = "bootstrap_owner_email is required when setup_enabled is true. Set setup_enabled=false for managed deployments that bootstrap organizations elsewhere."
    }

    precondition {
      condition     = length(local.control_environment_conflicts) == 0
      error_message = "control_environment must not set managed Helmr variables. Use explicit module inputs for control address, CAS URI, public URL, setup, bootstrap owner, and GitHub App settings."
    }

    precondition {
      condition     = var.enable_cloudfront || (var.public_url != null && (startswith(var.public_url, "https://") || (var.allow_insecure_http && startswith(var.public_url, "http://"))))
      error_message = "public_url must be HTTPS when enable_cloudfront is false, unless allow_insecure_http is explicitly enabled for development."
    }

    precondition {
      condition     = !var.enable_cloudfront || (var.certificate_arn != null && var.cloudfront_origin_domain_name != null && trimspace(var.cloudfront_origin_domain_name) != "")
      error_message = "enable_cloudfront requires certificate_arn and cloudfront_origin_domain_name so CloudFront can use a TLS ALB origin without pointing at its own viewer hostname."
    }

    precondition {
      condition     = var.private_control_dns_name == null || var.certificate_arn != null
      error_message = "certificate_arn is required when private_control_dns_name is set because workers must use HTTPS for registration credentials."
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

resource "aws_vpc_security_group_ingress_rule" "redis_control" {
  security_group_id            = aws_security_group.redis.id
  referenced_security_group_id = aws_security_group.control.id
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

resource "aws_vpc_security_group_ingress_rule" "postgres_control" {
  security_group_id            = aws_security_group.postgres.id
  referenced_security_group_id = aws_security_group.control.id
  from_port                    = 5432
  ip_protocol                  = "tcp"
  to_port                      = 5432
}

resource "aws_ecr_repository" "control" {
  count = var.create_control_repository ? 1 : 0

  name                 = "${local.name}/control"
  image_tag_mutability = "MUTABLE"
  force_delete         = var.control_repository_force_delete
  tags                 = var.tags

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_lifecycle_policy" "control" {
  count = var.create_control_repository && (var.control_ecr_max_images != null || var.control_ecr_untagged_image_expiration_days != null) ? 1 : 0

  repository = aws_ecr_repository.control[0].name

  policy = jsonencode({
    rules = concat(
      var.control_ecr_max_images == null ? [] : [{
        rulePriority = 1
        description  = "Keep the most recent tagged control images"
        selection = {
          tagStatus      = "tagged"
          tagPatternList = ["*"]
          countType      = "imageCountMoreThan"
          countNumber    = var.control_ecr_max_images
        }
        action = {
          type = "expire"
        }
      }],
      var.control_ecr_untagged_image_expiration_days == null ? [] : [{
        rulePriority = 2
        description  = "Expire untagged control images"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = var.control_ecr_untagged_image_expiration_days
        }
        action = {
          type = "expire"
        }
      }]
    )
  })
}

resource "aws_cloudwatch_log_group" "control" {
  name              = "/aws/ecs/${local.name}/control"
  retention_in_days = var.control_log_retention_days
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
  name        = "${local.name}-control-alb"
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

resource "aws_security_group" "control" {
  name        = "${local.name}-control"
  description = "Helmr control-plane tasks"
  vpc_id      = var.vpc_id
  tags        = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "control_alb" {
  security_group_id            = aws_security_group.control.id
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = local.control_port
  ip_protocol                  = "tcp"
  to_port                      = local.control_port
}

resource "aws_vpc_security_group_ingress_rule" "control_private_alb" {
  count                        = var.private_control_dns_name == null ? 0 : 1
  security_group_id            = aws_security_group.control.id
  referenced_security_group_id = aws_security_group.private_alb[0].id
  from_port                    = local.control_port
  ip_protocol                  = "tcp"
  to_port                      = local.control_port
}

resource "aws_vpc_security_group_egress_rule" "control" {
  security_group_id = aws_security_group.control.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_lb" "control" {
  name               = "${local.name}-control"
  load_balancer_type = "application"
  internal           = false
  security_groups    = [aws_security_group.alb.id]
  subnets            = var.public_subnet_ids
  tags               = var.tags
}

resource "aws_security_group" "private_alb" {
  count       = var.private_control_dns_name == null ? 0 : 1
  name        = "${local.name}-private-alb"
  description = "Helmr private worker-to-control load balancer"
  vpc_id      = var.vpc_id
  tags        = var.tags
}

resource "aws_vpc_security_group_ingress_rule" "private_alb_https" {
  count             = var.private_control_dns_name == null ? 0 : 1
  security_group_id = aws_security_group.private_alb[0].id
  cidr_ipv4         = data.aws_vpc.control.cidr_block
  from_port         = 443
  ip_protocol       = "tcp"
  to_port           = 443
}

resource "aws_vpc_security_group_egress_rule" "private_alb" {
  count             = var.private_control_dns_name == null ? 0 : 1
  security_group_id = aws_security_group.private_alb[0].id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_lb" "private_control" {
  count              = var.private_control_dns_name == null ? 0 : 1
  name               = "${local.name}-private"
  load_balancer_type = "application"
  internal           = true
  security_groups    = [aws_security_group.private_alb[0].id]
  subnets            = var.private_subnet_ids
  tags               = var.tags
}

resource "aws_lb_target_group" "control" {
  name        = "${local.name}-control"
  port        = local.control_port
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = var.vpc_id
  tags        = var.tags

  health_check {
    enabled             = true
    healthy_threshold   = 2
    interval            = 30
    matcher             = "200"
    path                = var.control_health_check_path
    timeout             = 5
    unhealthy_threshold = 2
  }
}

resource "aws_lb_target_group" "private_control" {
  count       = var.private_control_dns_name == null ? 0 : 1
  name        = "${local.name}-private"
  port        = local.control_port
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = var.vpc_id
  tags        = var.tags

  health_check {
    enabled             = true
    healthy_threshold   = 2
    interval            = 30
    matcher             = "200"
    path                = var.control_health_check_path
    timeout             = 5
    unhealthy_threshold = 2
  }
}

resource "aws_lb_listener" "http" {
  count             = !var.enable_cloudfront && var.allow_insecure_http ? 1 : 0
  load_balancer_arn = aws_lb.control.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.control.arn
  }
}

resource "aws_lb_listener" "http_redirect" {
  count             = var.enable_cloudfront || var.allow_insecure_http || var.certificate_arn == null ? 0 : 1
  load_balancer_arn = aws_lb.control.arn
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
  load_balancer_arn = aws_lb.control.arn
  port              = 443
  protocol          = "HTTPS"
  certificate_arn   = var.certificate_arn
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.control.arn
  }
}

resource "aws_lb_listener" "private_https" {
  count             = var.private_control_dns_name == null ? 0 : 1
  load_balancer_arn = aws_lb.private_control[0].arn
  port              = 443
  protocol          = "HTTPS"
  certificate_arn   = var.certificate_arn
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.private_control[0].arn
  }
}

resource "aws_route53_zone" "private_control" {
  count = var.private_control_dns_name == null ? 0 : 1

  name = var.private_control_dns_name

  vpc {
    vpc_id = var.vpc_id
  }

  tags = var.tags
}

resource "aws_route53_record" "private_control" {
  count = var.private_control_dns_name == null ? 0 : 1

  zone_id = aws_route53_zone.private_control[0].zone_id
  name    = var.private_control_dns_name
  type    = "A"

  alias {
    name                   = aws_lb.private_control[0].dns_name
    zone_id                = aws_lb.private_control[0].zone_id
    evaluate_target_health = true
  }
}

resource "aws_cloudfront_origin_request_policy" "control" {
  count = var.enable_cloudfront ? 1 : 0

  name    = "${local.name}-control"
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

resource "aws_cloudfront_distribution" "control" {
  count = var.enable_cloudfront ? 1 : 0

  enabled         = true
  is_ipv6_enabled = true
  comment         = "${local.name} Helmr control plane"
  price_class     = "PriceClass_100"
  tags            = var.tags

  origin {
    domain_name = var.cloudfront_origin_domain_name
    origin_id   = "control-alb"

    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "https-only"
      origin_ssl_protocols   = ["TLSv1.2"]
    }
  }

  default_cache_behavior {
    target_origin_id         = "control-alb"
    viewer_protocol_policy   = "redirect-to-https"
    allowed_methods          = ["DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT"]
    cached_methods           = ["GET", "HEAD"]
    compress                 = true
    cache_policy_id          = data.aws_cloudfront_cache_policy.caching_disabled[0].id
    origin_request_policy_id = aws_cloudfront_origin_request_policy.control[0].id
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

resource "aws_ecs_cluster" "control" {
  name = "${local.name}-control"
  tags = var.tags
}

resource "aws_iam_role" "control_execution" {
  name = "${local.name}-control-execution"
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

resource "aws_iam_role_policy_attachment" "control_execution" {
  role       = aws_iam_role.control_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
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

resource "aws_iam_role_policy_attachment" "dispatcher_execution" {
  role       = aws_iam_role.dispatcher_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "control_execution" {
  name = "${local.name}-control-execution"
  role = aws_iam_role.control_execution.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "secretsmanager:GetSecretValue"
        ]
        Resource = values(local.control_secrets)
      },
      {
        Effect = "Allow"
        Action = [
          "kms:Decrypt"
        ]
        Resource = aws_kms_key.helmr.arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = "secretsmanager.${data.aws_region.current.region}.amazonaws.com"
          }
        }
      }
    ]
  })
}

resource "aws_iam_role_policy" "dispatcher_execution" {
  name = "${local.name}-dispatcher-execution"
  role = aws_iam_role.dispatcher_execution.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
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
        Resource = aws_kms_key.helmr.arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = "secretsmanager.${data.aws_region.current.region}.amazonaws.com"
          }
        }
      }
    ]
  })
}

resource "aws_iam_role" "control_task" {
  name = "${local.name}-control-task"
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

resource "aws_iam_role_policy" "control_task" {
  name = "${local.name}-control-task"
  role = aws_iam_role.control_task.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
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
      }
    ]
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

resource "aws_ecs_task_definition" "control" {
  family                   = "${local.name}-control"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.control_cpu)
  memory                   = tostring(var.control_memory)
  execution_role_arn       = aws_iam_role.control_execution.arn
  task_role_arn            = aws_iam_role.control_task.arn
  tags                     = var.tags

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.control_architecture
  }

  container_definitions = jsonencode([{
    name       = "control"
    image      = var.control_image
    essential  = true
    entryPoint = var.control_entrypoint
    portMappings = [{
      containerPort = local.control_port
      hostPort      = local.control_port
      protocol      = "tcp"
    }]
    environment = [
      for key, value in local.control_environment : {
        name  = key
        value = value
      }
    ]
    secrets = [
      for key, value in local.control_secrets : {
        name      = key
        valueFrom = value
      }
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.control.name
        awslogs-region        = data.aws_region.current.region
        awslogs-stream-prefix = "control"
      }
    }
  }])
}

resource "aws_ecs_task_definition" "dispatcher" {
  family                   = "${local.name}-dispatcher"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.control_cpu)
  memory                   = tostring(var.control_memory)
  execution_role_arn       = aws_iam_role.dispatcher_execution.arn
  task_role_arn            = aws_iam_role.dispatcher_task.arn
  tags                     = var.tags

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.control_architecture
  }

  container_definitions = jsonencode([{
    name       = "dispatcher"
    image      = var.control_image
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
        awslogs-group         = aws_cloudwatch_log_group.control.name
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
  execution_role_arn       = aws_iam_role.control_execution.arn
  task_role_arn            = aws_iam_role.control_task.arn
  tags                     = var.tags

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.control_architecture
  }

  container_definitions = jsonencode([{
    name       = "migration"
    image      = var.control_image
    essential  = true
    entryPoint = var.control_entrypoint
    command    = ["migrate", "up"]
    secrets = [{
      name      = "HELMR_DATABASE_URL"
      valueFrom = aws_secretsmanager_secret.database_url.arn
    }]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.control.name
        awslogs-region        = data.aws_region.current.region
        awslogs-stream-prefix = "migration"
      }
    }
  }])
}

resource "aws_ecs_service" "control" {
  count = var.create_control_service ? 1 : 0

  name            = "control"
  cluster         = aws_ecs_cluster.control.id
  task_definition = aws_ecs_task_definition.control.arn
  desired_count   = var.control_desired_count
  launch_type     = "FARGATE"
  tags            = var.tags

  network_configuration {
    subnets          = local.control_subnet_ids
    security_groups  = [aws_security_group.control.id]
    assign_public_ip = var.control_assign_public_ip
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.control.arn
    container_name   = "control"
    container_port   = local.control_port
  }

  dynamic "load_balancer" {
    for_each = var.private_control_dns_name == null ? [] : [aws_lb_target_group.private_control[0].arn]

    content {
      target_group_arn = load_balancer.value
      container_name   = "control"
      container_port   = local.control_port
    }
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  depends_on = [
    aws_lb_listener.http,
    aws_lb_listener.http_redirect,
    aws_lb_listener.https,
    aws_lb_listener.private_https,
    aws_cloudfront_distribution.control
  ]

  lifecycle {
    precondition {
      condition     = var.certificate_arn != null || var.allow_insecure_http
      error_message = "certificate_arn is required when create_control_service is true unless allow_insecure_http is explicitly enabled."
    }

  }
}

resource "aws_ecs_service" "dispatcher" {
  count = var.create_control_service ? 1 : 0

  name            = "dispatcher"
  cluster         = aws_ecs_cluster.control.id
  task_definition = aws_ecs_task_definition.dispatcher.arn
  desired_count   = var.dispatcher_desired_count
  launch_type     = "FARGATE"
  tags            = var.tags

  network_configuration {
    subnets          = local.control_subnet_ids
    security_groups  = [aws_security_group.control.id]
    assign_public_ip = var.control_assign_public_ip
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

}

resource "aws_secretsmanager_secret" "database_url" {
  name                    = "${local.name}/control/database-url"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "worker_token_signing_key" {
  name                    = "${local.name}/control/worker-token-signing-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "auth_secret" {
  name                    = "${local.name}/control/auth-secret"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "secret_encryption_key" {
  name                    = "${local.name}/control/secret-encryption-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "github_app_private_key" {
  name                    = "${local.name}/control/github-app-private-key"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "github_app_webhook_secret" {
  name                    = "${local.name}/control/github-app-webhook-secret"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret" "github_app_client_secret" {
  name                    = "${local.name}/control/github-app-client-secret"
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

resource "aws_secretsmanager_secret" "worker_registration_token" {
  name                    = "${local.name}/worker-registration-token"
  kms_key_id              = aws_kms_key.helmr.arn
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}
