data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  partition  = data.aws_partition.current.partition
  dns_suffix = data.aws_partition.current.dns_suffix
  # ECR layer downloads are a separate, read-only bootstrap dependency.
  # CAS publication tags objects; reclamation enumerates and deletes exact versions.
  platform_s3_endpoint_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Principal = "*", Action = ["s3:GetObject", "s3:PutObject", "s3:PutObjectTagging", "s3:DeleteObject", "s3:ListBucketVersions", "s3:DeleteObjectVersion", "s3:ListBucket", "s3:GetBucketLocation", "s3:AbortMultipartUpload", "s3:ListBucketMultipartUploads", "s3:ListMultipartUploadParts"], Resource = [module.controlplane.cas_bucket_arn, "${module.controlplane.cas_bucket_arn}/*", var.platform_store_bucket_arn, "${var.platform_store_bucket_arn}/*"] },
      { Effect = "Allow", Principal = "*", Action = ["s3:GetObject"], Resource = ["arn:${local.partition}:s3:::prod-${var.aws_region}-starport-layer-bucket/*"] }
    ]
  })
  external_domains = concat(
    ["github.com", "api.github.com"],
    var.slack_transport == "socket" ? ["slack.com", "wss.slack.com", "wss-primary.slack.com", "wss-backup.slack.com"] : [],
    var.email_provider == "resend" ? ["api.resend.com"] : []
  )
  endpoint_services = toset(concat(["ecr.api", "ecr.dkr", "secretsmanager", "logs"], var.deployment_mode == "managed-cloud" ? ["kms"] : []))
  registry_host     = split("/", module.release_artifacts.controlplane_image)[0]
  clickhouse_host   = trimsuffix(trimprefix(var.clickhouse_url, "https://"), "/")
}

# Hosts use this path for control-plane calls. Guest packet filtering still denies
# the management and company prefixes before traffic reaches a VPC route.
resource "aws_vpc_peering_connection" "execution" {
  vpc_id      = module.controlplane_network.vpc_id
  peer_vpc_id = module.execution_network.vpc_id
  auto_accept = true
  requester { allow_remote_vpc_dns_resolution = true }
  accepter { allow_remote_vpc_dns_resolution = true }
  tags = local.tags
}
resource "aws_route" "execution_to_platform" {
  route_table_id            = module.execution_network.private_route_table_id
  destination_cidr_block    = var.controlplane_vpc_cidr
  vpc_peering_connection_id = aws_vpc_peering_connection.execution.id
}
resource "aws_route" "platform_to_execution" {
  route_table_id            = module.controlplane_network.private_route_table_id
  destination_cidr_block    = var.execution_vpc_cidr
  vpc_peering_connection_id = aws_vpc_peering_connection.execution.id
}

resource "aws_route53_zone" "application" {
  name = var.app_hostname
  vpc { vpc_id = module.controlplane_network.vpc_id }
  vpc { vpc_id = module.execution_network.vpc_id }
  dynamic "vpc" {
    for_each = toset(var.additional_application_vpc_ids)
    content { vpc_id = vpc.value }
  }
  tags = local.tags
}
resource "aws_route53_record" "application" {
  zone_id = aws_route53_zone.application.zone_id
  name    = var.app_hostname
  type    = "A"
  alias {
    name                   = module.controlplane.load_balancer_dns_name
    zone_id                = module.controlplane.load_balancer_zone_id
    evaluate_target_health = true
  }
}

resource "aws_security_group" "endpoints" {
  name   = "${var.name}-platform-endpoints"
  vpc_id = module.controlplane_network.vpc_id
  tags   = local.tags
}
resource "aws_vpc_security_group_ingress_rule" "endpoint_platform" {
  security_group_id            = aws_security_group.endpoints.id
  referenced_security_group_id = module.controlplane.controlplane_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}
resource "aws_vpc_security_group_ingress_rule" "endpoint_proxy" {
  security_group_id            = aws_security_group.endpoints.id
  referenced_security_group_id = aws_security_group.proxy.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}
resource "aws_vpc_endpoint" "platform" {
  for_each            = local.endpoint_services
  vpc_id              = module.controlplane_network.vpc_id
  service_name        = "com.amazonaws.${var.aws_region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  private_dns_enabled = true
  subnet_ids          = module.controlplane_network.private_subnet_ids
  security_group_ids  = [aws_security_group.endpoints.id]
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = "*", Action = "*", Resource = "*", Condition = { StringEquals = { "aws:PrincipalAccount" = data.aws_caller_identity.current.account_id } } }]
  })
  tags = local.tags
}
resource "aws_vpc_security_group_egress_rule" "platform_endpoints" {
  security_group_id            = module.controlplane.controlplane_security_group_id
  referenced_security_group_id = aws_security_group.endpoints.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}
resource "aws_vpc_security_group_egress_rule" "platform_s3" {
  security_group_id = module.controlplane.controlplane_security_group_id
  prefix_list_id    = module.controlplane_network.s3_prefix_list_id
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}
resource "aws_vpc_security_group_egress_rule" "platform_postgres" {
  security_group_id            = module.controlplane.controlplane_security_group_id
  referenced_security_group_id = module.controlplane.postgres_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}
resource "aws_vpc_security_group_egress_rule" "platform_self" {
  security_group_id            = module.controlplane.controlplane_security_group_id
  referenced_security_group_id = module.controlplane.load_balancer_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
}
resource "aws_vpc_security_group_egress_rule" "platform_clickhouse" {
  security_group_id            = module.controlplane.controlplane_security_group_id
  referenced_security_group_id = var.clickhouse_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = var.clickhouse_port
  to_port                      = var.clickhouse_port
}

# Route53's VPC resolver is not filtered by security groups. A fail-closed
# resolver policy prevents arbitrary queries from becoming an alternate exit.
resource "aws_route53_resolver_firewall_domain_list" "allowed" {
  name = "${var.name}-platform-allowed"
  domains = [for domain in concat(local.external_domains, [
    var.app_hostname, aws_service_discovery_private_dns_namespace.proxy.name,
    "*.${aws_service_discovery_private_dns_namespace.proxy.name}",
    module.controlplane.postgres_hostname,
    split(":", local.clickhouse_host)[0],
    local.registry_host,
    split("/", var.proxy_image)[0],
    "api.ecr.${var.aws_region}.${local.dns_suffix}",
    "secretsmanager.${var.aws_region}.${local.dns_suffix}",
    "logs.${var.aws_region}.${local.dns_suffix}",
    "kms.${var.aws_region}.${local.dns_suffix}",
    "s3.${var.aws_region}.${local.dns_suffix}",
    "${module.controlplane.cas_bucket_name}.s3.${var.aws_region}.${local.dns_suffix}",
    "${trimprefix(var.platform_store_bucket_arn, "arn:${local.partition}:s3:::")}.s3.${var.aws_region}.${local.dns_suffix}",
    "prod-${var.aws_region}-starport-layer-bucket.s3.${var.aws_region}.${local.dns_suffix}"
  ]) : "${trimsuffix(domain, ".")}."]
}
resource "aws_route53_resolver_firewall_domain_list" "all" {
  name    = "${var.name}-platform-all"
  domains = ["*."]
}
resource "aws_route53_resolver_firewall_rule_group" "platform" {
  name = "${var.name}-platform"
  tags = local.tags
}
# Trust provider-controlled CNAME chains only within an approved-name query.
# Direct queries for an unlisted redirection target still hit the blocking rule.
resource "aws_route53_resolver_firewall_rule" "allow" {
  firewall_domain_redirection_action = "TRUST_REDIRECTION_DOMAIN"
  name                               = "approved-platform-services"
  firewall_rule_group_id             = aws_route53_resolver_firewall_rule_group.platform.id
  firewall_domain_list_id            = aws_route53_resolver_firewall_domain_list.allowed.id
  priority                           = 100
  action                             = "ALLOW"
}
resource "aws_route53_resolver_firewall_rule" "block" {
  name                    = "block-other-domains"
  firewall_rule_group_id  = aws_route53_resolver_firewall_rule_group.platform.id
  firewall_domain_list_id = aws_route53_resolver_firewall_domain_list.all.id
  priority                = 200
  action                  = "BLOCK"
  block_response          = "NXDOMAIN"
}
resource "aws_route53_resolver_firewall_rule_group_association" "platform" {
  name                   = "${var.name}-platform"
  firewall_rule_group_id = aws_route53_resolver_firewall_rule_group.platform.id
  vpc_id                 = module.controlplane_network.vpc_id
  priority               = 101
}
resource "aws_route53_resolver_firewall_config" "platform" {
  resource_id        = module.controlplane_network.vpc_id
  firewall_fail_open = "DISABLED"
}
