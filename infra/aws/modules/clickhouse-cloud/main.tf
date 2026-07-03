locals {
  name                    = lower(var.name)
  clickhouse_region       = coalesce(var.clickhouse_region, data.aws_region.current.region)
  service_name            = coalesce(var.service_name, "${local.name}-telemetry")
  private_dns_hostname    = clickhouse_service.telemetry.private_endpoint_config.private_dns_hostname
  private_dns_hostname_id = trimsuffix(local.private_dns_hostname, ".")
}

resource "random_password" "clickhouse" {
  length  = 32
  special = false
}

resource "aws_secretsmanager_secret" "clickhouse_password" {
  name                    = "${local.name}/clickhouse/password"
  kms_key_id              = var.secret_kms_key_id
  recovery_window_in_days = var.secret_recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret_version" "clickhouse_password" {
  secret_id     = aws_secretsmanager_secret.clickhouse_password.id
  secret_string = random_password.clickhouse.result
}

resource "clickhouse_service" "telemetry" {
  name            = local.service_name
  cloud_provider  = "aws"
  region          = local.clickhouse_region
  release_channel = var.release_channel

  idle_scaling         = var.idle_scaling
  idle_timeout_minutes = var.idle_scaling ? var.idle_timeout_minutes : null

  password_hash = base64sha256(random_password.clickhouse.result)
  ip_access     = []

  min_replica_memory_gb = var.min_replica_memory_gb
  max_replica_memory_gb = var.max_replica_memory_gb

  backup_configuration = {
    backup_period_in_hours           = var.backup_period_in_hours
    backup_retention_period_in_hours = var.backup_retention_period_in_hours
    backup_start_time                = null
  }

  lifecycle {
    precondition {
      condition     = local.clickhouse_region == data.aws_region.current.region
      error_message = "clickhouse_region must match the AWS provider region; create a separate regional stack for cross-region connectivity."
    }
  }
}

resource "aws_security_group" "client" {
  name        = "${local.name}-clickhouse-client"
  description = "Helmr clients allowed to reach ClickHouse Cloud PrivateLink"
  vpc_id      = var.vpc_id
  tags        = merge(var.tags, { Name = "${local.name}-clickhouse-client" })
}

resource "aws_security_group" "endpoint" {
  name        = "${local.name}-clickhouse-endpoint"
  description = "ClickHouse Cloud PrivateLink endpoint"
  vpc_id      = var.vpc_id
  tags        = merge(var.tags, { Name = "${local.name}-clickhouse-endpoint" })
}

resource "aws_vpc_security_group_ingress_rule" "endpoint_https" {
  security_group_id            = aws_security_group.endpoint.id
  referenced_security_group_id = aws_security_group.client.id
  from_port                    = var.https_port
  ip_protocol                  = "tcp"
  to_port                      = var.https_port
}

resource "aws_vpc_security_group_egress_rule" "endpoint" {
  security_group_id = aws_security_group.endpoint.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_vpc_endpoint" "clickhouse" {
  vpc_id              = var.vpc_id
  service_name        = clickhouse_service.telemetry.private_endpoint_config.endpoint_service_id
  vpc_endpoint_type   = "Interface"
  subnet_ids          = var.subnet_ids
  security_group_ids  = [aws_security_group.endpoint.id]
  private_dns_enabled = false
  tags                = merge(var.tags, { Name = "${local.name}-clickhouse" })
}

resource "clickhouse_service_private_endpoints_attachment" "telemetry" {
  service_id           = clickhouse_service.telemetry.id
  private_endpoint_ids = [aws_vpc_endpoint.clickhouse.id]
}

resource "aws_route53_zone" "clickhouse" {
  name = local.private_dns_hostname_id

  vpc {
    vpc_id = var.vpc_id
  }

  tags = merge(var.tags, { Name = "${local.name}-clickhouse" })
}

resource "aws_route53_record" "clickhouse" {
  zone_id = aws_route53_zone.clickhouse.zone_id
  name    = local.private_dns_hostname_id
  type    = "A"

  alias {
    name                   = aws_vpc_endpoint.clickhouse.dns_entry[0].dns_name
    zone_id                = aws_vpc_endpoint.clickhouse.dns_entry[0].hosted_zone_id
    evaluate_target_health = false
  }
}
