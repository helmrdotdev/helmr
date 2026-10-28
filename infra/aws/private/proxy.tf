locals {
  proxy_pin    = jsondecode(file("${path.module}/proxy-image.json"))
  proxy_config = templatefile("${path.module}/templates/squid.conf.tftpl", { domains = local.external_domains })
}
resource "aws_security_group" "proxy" {
  name   = "${var.name}-platform-proxy"
  vpc_id = module.controlplane_network.vpc_id
  tags   = local.tags
}
resource "aws_vpc_security_group_ingress_rule" "proxy" {
  security_group_id            = aws_security_group.proxy.id
  referenced_security_group_id = module.controlplane.controlplane_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = 3128
  to_port                      = 3128
}
resource "aws_vpc_security_group_egress_rule" "platform_proxy" {
  security_group_id            = module.controlplane.controlplane_security_group_id
  referenced_security_group_id = aws_security_group.proxy.id
  ip_protocol                  = "tcp"
  from_port                    = 3128
  to_port                      = 3128
}
resource "aws_vpc_security_group_egress_rule" "proxy_https" {
  security_group_id = aws_security_group.proxy.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}
resource "aws_service_discovery_private_dns_namespace" "proxy" {
  name = "${var.name}.platform.internal"
  vpc  = module.controlplane_network.vpc_id
  tags = local.tags
}
resource "aws_service_discovery_service" "proxy" {
  name = "proxy"
  dns_config {
    namespace_id = aws_service_discovery_private_dns_namespace.proxy.id
    dns_records {
      type = "A"
      ttl  = 10
    }
    routing_policy = "MULTIVALUE"
  }
  health_check_custom_config { failure_threshold = 1 }
}
resource "aws_cloudwatch_log_group" "proxy" {
  name              = "/helmr/${var.name}/platform-proxy"
  retention_in_days = var.controlplane_log_retention_days
  tags              = local.tags
}
resource "aws_iam_role" "proxy_execution" {
  name               = "${var.name}-proxy-execution"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "ecs-tasks.amazonaws.com" }, Action = "sts:AssumeRole" }] })
  tags               = local.tags
}
resource "aws_iam_role_policy" "proxy_execution" {
  role = aws_iam_role.proxy_execution.id
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Effect = "Allow", Action = ["ecr:GetAuthorizationToken"], Resource = "*" },
    { Effect = "Allow", Action = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability"], Resource = var.proxy_image_repository_arn },
    { Effect = "Allow", Action = ["logs:CreateLogStream", "logs:PutLogEvents"], Resource = "${aws_cloudwatch_log_group.proxy.arn}:*" }
  ] })
}
resource "aws_ecs_task_definition" "proxy" {
  family                   = "${var.name}-platform-proxy"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = aws_iam_role.proxy_execution.arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }
  volume { name = "scratch" }
  container_definitions = jsonencode([{
    name             = "proxy", image = var.proxy_image, essential = true, user = "13:13", readonlyRootFilesystem = true,
    entryPoint       = ["/bin/sh", "-ec"], command = [file("${path.module}/templates/proxy-start.sh")],
    environment      = [{ name = "SQUID_CONFIG", value = base64encode(local.proxy_config) }],
    mountPoints      = [{ sourceVolume = "scratch", containerPath = "/var/spool/squid", readOnly = false }],
    portMappings     = [{ containerPort = 3128, protocol = "tcp" }],
    linuxParameters  = { capabilities = { add = [], drop = ["ALL"] } },
    logConfiguration = { logDriver = "awslogs", options = { awslogs-group = aws_cloudwatch_log_group.proxy.name, awslogs-region = var.aws_region, awslogs-stream-prefix = "proxy" } },
    healthCheck      = { command = ["CMD-SHELL", "/usr/sbin/squid -k check -f /var/spool/squid/squid.conf"], interval = 30, timeout = 5, retries = 3, startPeriod = 20 }
  }])
  lifecycle {
    precondition {
      condition     = endswith(var.proxy_image, "@${local.proxy_pin.digest}")
      error_message = "Mirror the tested Squid manifest without changing its digest, or update the checked-in pin and qualify the replacement."
    }
  }
  tags = local.tags
}
resource "aws_ecs_service" "proxy" {
  count           = var.create_controlplane_service ? 1 : 0
  name            = "${var.name}-platform-proxy"
  cluster         = module.controlplane.controlplane_cluster_arn
  task_definition = aws_ecs_task_definition.proxy.arn
  desired_count   = 2
  launch_type     = "FARGATE"
  deployment_circuit_breaker {
    enable   = true
    rollback = var.enable_deployment_rollback
  }
  network_configuration {
    subnets          = module.controlplane_network.private_subnet_ids
    security_groups  = [aws_security_group.proxy.id]
    assign_public_ip = false
  }
  service_registries { registry_arn = aws_service_discovery_service.proxy.arn }
  depends_on = [aws_vpc_endpoint.platform, aws_route53_resolver_firewall_rule_group_association.platform]
  tags       = local.tags
}
