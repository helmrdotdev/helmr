locals {
  name = lower(var.name)
  groups = [for group in var.groups : {
    worker_group_id                 = group.worker_group_id
    autoscaling_group_name          = group.autoscaling_group_name
    termination_lifecycle_hook_name = group.termination_lifecycle_hook_name
    allows_run                      = group.allows_run
    allows_build                    = group.allows_build
    instance_capacity               = group.instance_capacity
  }]
  asg_arns = [for group in var.groups : group.autoscaling_group_arn]
}

resource "aws_cloudwatch_log_group" "capacity" {
  count = var.enabled ? 1 : 0

  name              = "/helmr/${local.name}/capacity"
  retention_in_days = var.log_retention_days
  tags              = var.tags
}

resource "aws_security_group" "capacity" {
  count = var.enabled ? 1 : 0

  name_prefix = "${local.name}-capacity-"
  description = "Egress for the one-shot Helmr capacity task"
  vpc_id      = var.vpc_id
  tags        = var.tags

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_iam_role" "execution" {
  count = var.enabled ? 1 : 0

  name = "${local.name}-capacity-execution"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "execution" {
  count = var.enabled ? 1 : 0

  name = "${local.name}-capacity-execution"
  role = aws_iam_role.execution[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "WriteLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "${aws_cloudwatch_log_group.capacity[0].arn}:*"
      },
      {
        Sid      = "ReadOperatorToken"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = var.operator_token_secret_arn
      },
      {
        Sid      = "DecryptOperatorToken"
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = var.operator_token_kms_key_arn
        Condition = {
          StringEquals = {
            "kms:ViaService" = "secretsmanager.${data.aws_region.current.region}.amazonaws.com"
          }
        }
      },
      {
        Sid    = "PullCapacityImage"
        Effect = "Allow"
        Action = [
          "ecr:BatchCheckLayerAvailability",
          "ecr:GetDownloadUrlForLayer",
          "ecr:BatchGetImage"
        ]
        Resource = var.control_image_repository_arn
      },
      {
        Sid      = "GetRegistryToken"
        Effect   = "Allow"
        Action   = ["ecr:GetAuthorizationToken"]
        Resource = "*"
      }
    ]
  })
}

resource "aws_iam_role" "task" {
  count = var.enabled ? 1 : 0

  name = "${local.name}-capacity-task"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "task" {
  count = var.enabled ? 1 : 0

  name = "${local.name}-capacity-task"
  role = aws_iam_role.task[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "DescribeConfiguredGroups"
        Effect   = "Allow"
        Action   = ["autoscaling:DescribeAutoScalingGroups"]
        Resource = "*"
      },
      {
        Sid    = "MutateConfiguredGroups"
        Effect = "Allow"
        Action = [
          "autoscaling:SetDesiredCapacity",
          "autoscaling:TerminateInstanceInAutoScalingGroup",
          "autoscaling:CompleteLifecycleAction"
        ]
        Resource = local.asg_arns
      }
    ]
  })
}

resource "aws_ecs_task_definition" "capacity" {
  count = var.enabled ? 1 : 0

  family                   = "${local.name}-capacity"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = tostring(var.task_cpu)
  memory                   = tostring(var.task_memory)
  execution_role_arn       = aws_iam_role.execution[0].arn
  task_role_arn            = aws_iam_role.task[0].arn
  tags                     = var.tags

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }

  container_definitions = jsonencode([{
    name       = "capacity"
    image      = var.control_image
    essential  = true
    entryPoint = ["helmr-aws-capacity"]
    environment = [
      { name = "HELMR_CONTROL_URL", value = var.control_url },
      { name = "HELMR_CAPACITY_GROUPS", value = jsonencode(local.groups) },
      { name = "HELMR_CAPACITY_OBSERVATION_MAX_AGE", value = var.observation_max_age },
      { name = "HELMR_CAPACITY_RECONCILE_TIMEOUT", value = var.reconcile_timeout }
    ]
    secrets = [{
      name      = "HELMR_OPERATOR_TOKEN"
      valueFrom = var.operator_token_secret_arn
    }]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.capacity[0].name
        awslogs-region        = data.aws_region.current.region
        awslogs-stream-prefix = "capacity"
      }
    }
  }])
}

resource "aws_iam_role" "scheduler" {
  count = var.enabled ? 1 : 0

  name = "${local.name}-capacity-scheduler"
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "scheduler.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "scheduler" {
  count = var.enabled ? 1 : 0

  name = "${local.name}-capacity-scheduler"
  role = aws_iam_role.scheduler[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["ecs:RunTask"]
        Resource = aws_ecs_task_definition.capacity[0].arn
        Condition = {
          ArnEquals = { "ecs:cluster" = var.ecs_cluster_arn }
        }
      },
      {
        Effect = "Allow"
        Action = ["iam:PassRole"]
        Resource = [
          aws_iam_role.execution[0].arn,
          aws_iam_role.task[0].arn
        ]
      }
    ]
  })
}

resource "aws_scheduler_schedule" "capacity" {
  count = var.enabled ? 1 : 0

  name                = "${local.name}-capacity"
  schedule_expression = var.schedule_expression
  state               = "ENABLED"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = var.ecs_cluster_arn
    role_arn = aws_iam_role.scheduler[0].arn

    retry_policy {
      maximum_event_age_in_seconds = 300
      maximum_retry_attempts       = 2
    }

    ecs_parameters {
      task_definition_arn = aws_ecs_task_definition.capacity[0].arn
      launch_type         = "FARGATE"
      task_count          = 1

      network_configuration {
        assign_public_ip = var.assign_public_ip
        security_groups  = [aws_security_group.capacity[0].id]
        subnets          = var.subnet_ids
      }
    }
  }
}
