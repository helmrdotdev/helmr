mock_provider "aws" {
  mock_data "aws_region" {
    defaults = {
      region = "us-east-1"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::111122223333:role/helmr-test-capacity"
    }
  }

  mock_resource "aws_ecs_task_definition" {
    defaults = {
      arn = "arn:aws:ecs:us-east-1:111122223333:task-definition/helmr-test-capacity:1"
    }
  }

  mock_resource "aws_security_group" {
    defaults = {
      id = "sg-00000000000000000"
    }
  }
}

variables {
  name                         = "helmr-test"
  enabled                      = true
  vpc_id                       = "vpc-00000000000000000"
  subnet_ids                   = ["subnet-00000000000000000"]
  assign_public_ip             = false
  ecs_cluster_arn              = "arn:aws:ecs:us-east-1:111122223333:cluster/helmr-test-control"
  control_url                  = "https://control.example.test"
  operator_token_secret_arn    = "arn:aws:secretsmanager:us-east-1:111122223333:secret:operator-token"
  operator_token_kms_key_arn   = "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000"
  control_image                = "111122223333.dkr.ecr.us-east-1.amazonaws.com/helmr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  control_image_repository_arn = "arn:aws:ecr:us-east-1:111122223333:repository/helmr"
  groups = [{
    worker_group_id                 = "run-workers"
    autoscaling_group_name          = "helmr-run"
    autoscaling_group_arn           = "arn:aws:autoscaling:us-east-1:111122223333:autoScalingGroup:00000000-0000-0000-0000-000000000000:autoScalingGroupName/helmr-run"
    termination_lifecycle_hook_name = "helmr-run-terminate"
    allows_run                      = true
    allows_build                    = false
    instance_capacity = {
      cpu_millis                 = 8000
      memory_bytes               = 34359738368
      guest_ephemeral_disk_bytes = 137438953472
      vm_slots                   = 4
      run_consumers              = 4
      build_executors            = 0
    }
  }]
}

run "one_shot_http_capacity_boundary" {
  command = plan

  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.capacity[0].container_definitions)[0].entryPoint == ["helmr-aws-capacity"] &&
      contains([
        for secret in jsondecode(aws_ecs_task_definition.capacity[0].container_definitions)[0].secrets : secret.name
      ], "HELMR_OPERATOR_TOKEN") &&
      !contains([
        for secret in jsondecode(aws_ecs_task_definition.capacity[0].container_definitions)[0].secrets : secret.name
      ], "HELMR_DATABASE_URL")
    )
    error_message = "capacity task must use the operator credential and must not receive a Control DB credential"
  }

  assert {
    condition = (
      aws_scheduler_schedule.capacity[0].target[0].ecs_parameters[0].task_count == 1 &&
      aws_scheduler_schedule.capacity[0].target[0].ecs_parameters[0].launch_type == "FARGATE"
    )
    error_message = "Managed Cloud capacity automation must remain a one-shot scheduled ECS task"
  }

  assert {
    condition = toset(one([
      for statement in jsondecode(aws_iam_role_policy.task[0].policy).Statement : statement.Resource
      if statement.Sid == "MutateConfiguredGroups"
    ])) == toset([var.groups[0].autoscaling_group_arn])
    error_message = "capacity mutation must be scoped to configured Auto Scaling groups"
  }
}
