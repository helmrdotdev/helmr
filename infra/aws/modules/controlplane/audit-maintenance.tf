# Purge has its own credential and execution identity. Serving tasks never receive
# delete authority. A missing successful run alarms even when ECS cannot start.
resource "aws_cloudwatch_log_group" "audit_maintenance" {
  name              = "/helmr/${local.name}/audit-maintenance"
  retention_in_days = 30
  tags              = var.tags
}

resource "aws_iam_role" "audit_maintenance_execution" {
  name                 = "${local.name}-audit-maintenance-execution"
  permissions_boundary = var.permissions_boundary_arn
  tags                 = var.tags
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ecs-tasks.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_iam_role_policy" "audit_maintenance_execution" {
  role = aws_iam_role.audit_maintenance_execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      { Effect = "Allow", Action = ["logs:CreateLogStream", "logs:PutLogEvents"], Resource = "${aws_cloudwatch_log_group.audit_maintenance.arn}:*" },
      { Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = aws_secretsmanager_secret.database_audit_maintenance_url.arn },
      { Effect = "Allow", Action = ["kms:Decrypt"], Resource = aws_kms_key.helmr.arn, Condition = { StringEquals = { "kms:ViaService" = "secretsmanager.${data.aws_region.current.region}.amazonaws.com" } } }
      ], var.controlplane_image_repository_arn == null ? [] : [
      { Effect = "Allow", Action = ["ecr:GetAuthorizationToken"], Resource = "*" },
      { Effect = "Allow", Action = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"], Resource = var.controlplane_image_repository_arn }
    ])
  })
}

resource "aws_ecs_task_definition" "audit_maintenance" {
  family                   = "${local.name}-audit-maintenance"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = aws_iam_role.audit_maintenance_execution.arn
  tags                     = var.tags
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.controlplane_architecture
  }
  container_definitions = jsonencode([{
    name       = "audit-maintenance"
    image      = var.controlplane_image
    essential  = true
    entryPoint = var.controlplane_entrypoint
    command    = ["audit", "purge"]
    environment = [
      { name = "AUDIT_RETENTION_SECONDS", value = tostring(var.audit_retention_seconds) },
      { name = "AUDIT_MAINTENANCE_TIMEOUT_SECONDS", value = tostring(var.audit_maintenance_timeout_seconds) },
      { name = "AUDIT_PURGE_MAX_BATCHES", value = tostring(var.audit_purge_max_batches) }
    ]
    secrets = [{ name = "DATABASE_AUDIT_MAINTENANCE_URL", valueFrom = aws_secretsmanager_secret.database_audit_maintenance_url.arn }]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.audit_maintenance.name
        awslogs-region        = data.aws_region.current.region
        awslogs-stream-prefix = "purge"
      }
    }
  }])
  lifecycle {
    precondition {
      condition     = var.audit_maintenance_timeout_seconds < var.audit_purge_interval_minutes * 60 && var.audit_purge_max_batches * 5 + 15 <= var.audit_maintenance_timeout_seconds
      error_message = "Audit maintenance timeout must cover all five-second batch deadlines plus 15 seconds of setup overhead and be shorter than the purge interval."
    }
  }
}

resource "aws_cloudwatch_event_rule" "audit_purge" {
  name                = "${local.name}-audit-purge"
  schedule_expression = "rate(${var.audit_purge_interval_minutes} minutes)"
  state               = var.create_controlplane_service ? "ENABLED" : "DISABLED"
  tags                = var.tags
}

resource "aws_iam_role" "audit_purge_schedule" {
  name                 = "${local.name}-audit-purge-schedule"
  permissions_boundary = var.permissions_boundary_arn
  tags                 = var.tags
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow", Principal = { Service = "events.amazonaws.com" }, Action = "sts:AssumeRole"
      Condition = { ArnEquals = { "aws:SourceArn" = aws_cloudwatch_event_rule.audit_purge.arn } }
    }]
  })
}

resource "aws_iam_role_policy" "audit_purge_schedule" {
  role = aws_iam_role.audit_purge_schedule.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["ecs:RunTask"], Resource = aws_ecs_task_definition.audit_maintenance.arn, Condition = { ArnEquals = { "ecs:cluster" = aws_ecs_cluster.controlplane.arn } } },
      { Effect = "Allow", Action = ["iam:PassRole"], Resource = aws_iam_role.audit_maintenance_execution.arn, Condition = { StringEquals = { "iam:PassedToService" = "ecs-tasks.amazonaws.com" } } }
    ]
  })
}

resource "aws_cloudwatch_event_target" "audit_purge" {
  rule     = aws_cloudwatch_event_rule.audit_purge.name
  arn      = aws_ecs_cluster.controlplane.arn
  role_arn = aws_iam_role.audit_purge_schedule.arn
  ecs_target {
    task_definition_arn = aws_ecs_task_definition.audit_maintenance.arn
    launch_type         = "FARGATE"
    task_count          = 1
    network_configuration {
      subnets          = local.controlplane_subnet_ids
      security_groups  = local.controlplane_security_group_ids
      assign_public_ip = var.controlplane_assign_public_ip
    }
  }
  retry_policy {
    maximum_event_age_in_seconds = 300
    maximum_retry_attempts       = 1
  }
  depends_on = [aws_iam_role_policy.audit_purge_schedule]
}

resource "aws_cloudwatch_log_metric_filter" "audit_purge_success" {
  name           = "purge-success"
  log_group_name = aws_cloudwatch_log_group.audit_maintenance.name
  pattern        = "{ $.batches = * && $.deleted = * }"
  metric_transformation {
    name      = "PurgeSucceeded"
    namespace = "Helmr/${local.name}"
    value     = "1"
  }
}

resource "aws_cloudwatch_metric_alarm" "audit_purge_missing" {
  count               = var.create_controlplane_service ? 1 : 0
  alarm_name          = "${local.name}-audit-purge-missing"
  alarm_description   = "No successful bounded audit purge within two scheduled intervals. Check task launch, database role, timeout and purge logs."
  namespace           = "Helmr/${local.name}"
  metric_name         = "PurgeSucceeded"
  statistic           = "Sum"
  period              = var.audit_purge_interval_minutes * 120
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = var.audit_maintenance_alarm_actions
  tags                = var.tags
}

output "audit_maintenance_task_definition_arn" {
  value = aws_ecs_task_definition.audit_maintenance.arn
}
